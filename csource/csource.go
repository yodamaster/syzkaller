// Copyright 2015 syzkaller project authors. All rights reserved.
// Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.

// I heard you like shell...
//go:generate bash -c "echo -e '// AUTOGENERATED FROM executor/common.h\npackage csource\nvar commonHeader = `' > common.go; cat ../executor/common.h | sed -e '/#include \"common_kvm_amd64.h\"/ {' -e 'r ../executor/common_kvm_amd64.h' -e 'd' -e '}' - | sed -e '/#include \"common_kvm_arm64.h\"/ {' -e 'r ../executor/common_kvm_arm64.h' -e 'd' -e '}' - | sed -e '/#include \"kvm.h\"/ {' -e 'r ../executor/kvm.h' -e 'd' -e '}' - | sed -e '/#include \"kvm.S.h\"/ {' -e 'r ../executor/kvm.S.h' -e 'd' -e '}' - | egrep -v '^[   ]*//' | sed '/^[ 	]*\\/\\/.*/d' | sed 's#[ 	]*//.*##g' >> common.go; echo '`' >> common.go"
//go:generate go fmt common.go

package csource

import (
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"strings"
	"unsafe"

	"github.com/google/syzkaller/prog"
	"github.com/google/syzkaller/sys"
)

type Options struct {
	Threaded bool
	Collide  bool
	Repeat   bool
	Procs    int
	Sandbox  string
	Repro    bool // generate code for use with repro package
}

func Write(p *prog.Prog, opts Options) ([]byte, error) {
	exec := make([]byte, prog.ExecBufferSize)
	if err := p.SerializeForExec(exec, 0); err != nil {
		return nil, fmt.Errorf("failed to serialize program: %v", err)
	}
	w := new(bytes.Buffer)

	fmt.Fprint(w, "// autogenerated by syzkaller (http://github.com/google/syzkaller)\n\n")

	handled := make(map[string]int)
	for _, c := range p.Calls {
		handled[c.Meta.CallName] = c.Meta.NR
	}
	for name, nr := range handled {
		fmt.Fprintf(w, "#ifndef __NR_%v\n", name)
		fmt.Fprintf(w, "#define __NR_%v %v\n", name, nr)
		fmt.Fprintf(w, "#endif\n")
	}
	fmt.Fprintf(w, "\n")

	enableTun := "false"
	if _, ok := handled["syz_emit_ethernet"]; ok {
		enableTun = "true"
	}

	hdr, err := preprocessCommonHeader(opts, handled)
	if err != nil {
		return nil, err
	}
	fmt.Fprint(w, hdr)
	fmt.Fprint(w, "\n")

	calls, nvar := generateCalls(exec)
	fmt.Fprintf(w, "long r[%v];\n", nvar)

	if !opts.Repeat {
		generateTestFunc(w, opts, calls, "loop")

		fmt.Fprint(w, "int main()\n{\n")
		fmt.Fprintf(w, "\tsetup_main_process();\n")
		fmt.Fprintf(w, "\tint pid = do_sandbox_%v(0, %v);\n", opts.Sandbox, enableTun)
		fmt.Fprint(w, "\tint status = 0;\n")
		fmt.Fprint(w, "\twhile (waitpid(pid, &status, __WALL) != pid) {}\n")
		fmt.Fprint(w, "\treturn 0;\n}\n")
	} else {
		generateTestFunc(w, opts, calls, "test")
		if opts.Procs <= 1 {
			fmt.Fprint(w, "int main()\n{\n")
			fmt.Fprintf(w, "\tsetup_main_process();\n")
			fmt.Fprintf(w, "\tint pid = do_sandbox_%v(0, %v);\n", opts.Sandbox, enableTun)
			fmt.Fprint(w, "\tint status = 0;\n")
			fmt.Fprint(w, "\twhile (waitpid(pid, &status, __WALL) != pid) {}\n")
			fmt.Fprint(w, "\treturn 0;\n}\n")
		} else {
			fmt.Fprint(w, "int main()\n{\n")
			fmt.Fprint(w, "\tint i;")
			fmt.Fprintf(w, "\tfor (i = 0; i < %v; i++) {\n", opts.Procs)
			fmt.Fprint(w, "\t\tif (fork() == 0) {\n")
			fmt.Fprintf(w, "\t\t\tsetup_main_process();\n")
			fmt.Fprintf(w, "\t\t\tint pid = do_sandbox_%v(i, %v);\n", opts.Sandbox, enableTun)
			fmt.Fprint(w, "\t\t\tint status = 0;\n")
			fmt.Fprint(w, "\t\t\twhile (waitpid(pid, &status, __WALL) != pid) {}\n")
			fmt.Fprint(w, "\t\t\treturn 0;\n")
			fmt.Fprint(w, "\t\t}\n")
			fmt.Fprint(w, "\t}\n")
			fmt.Fprint(w, "\tsleep(1000000);\n")
			fmt.Fprint(w, "\treturn 0;\n}\n")
		}
	}
	// Remove duplicate new lines.
	out := w.Bytes()
	for {
		out1 := bytes.Replace(out, []byte{'\n', '\n', '\n'}, []byte{'\n', '\n'}, -1)
		if len(out) == len(out1) {
			break
		}
		out = out1
	}
	return out, nil
}

func generateTestFunc(w io.Writer, opts Options, calls []string, name string) {
	if !opts.Threaded && !opts.Collide {
		fmt.Fprintf(w, "void %v()\n{\n", name)
		if opts.Repro {
			fmt.Fprintf(w, "\tsyscall(SYS_write, 1, \"executing program\\n\", strlen(\"executing program\\n\"));\n")
		}
		fmt.Fprintf(w, "\tmemset(r, -1, sizeof(r));\n")
		for _, c := range calls {
			fmt.Fprintf(w, "%s", c)
		}
		fmt.Fprintf(w, "}\n")
	} else {
		fmt.Fprintf(w, "void *thr(void *arg)\n{\n")
		fmt.Fprintf(w, "\tswitch ((long)arg) {\n")
		for i, c := range calls {
			fmt.Fprintf(w, "\tcase %v:\n", i)
			fmt.Fprintf(w, "%s", strings.Replace(c, "\t", "\t\t", -1))
			fmt.Fprintf(w, "\t\tbreak;\n")
		}
		fmt.Fprintf(w, "\t}\n")
		fmt.Fprintf(w, "\treturn 0;\n}\n\n")

		fmt.Fprintf(w, "void %v()\n{\n", name)
		fmt.Fprintf(w, "\tlong i;\n")
		fmt.Fprintf(w, "\tpthread_t th[%v];\n", 2*len(calls))
		fmt.Fprintf(w, "\n")
		if opts.Repro {
			fmt.Fprintf(w, "\tsyscall(SYS_write, 1, \"executing program\\n\", strlen(\"executing program\\n\"));\n")
		}
		fmt.Fprintf(w, "\tmemset(r, -1, sizeof(r));\n")
		fmt.Fprintf(w, "\tsrand(getpid());\n")
		fmt.Fprintf(w, "\tfor (i = 0; i < %v; i++) {\n", len(calls))
		fmt.Fprintf(w, "\t\tpthread_create(&th[i], 0, thr, (void*)i);\n")
		fmt.Fprintf(w, "\t\tusleep(10000);\n")
		fmt.Fprintf(w, "\t}\n")
		if opts.Collide {
			fmt.Fprintf(w, "\tfor (i = 0; i < %v; i++) {\n", len(calls))
			fmt.Fprintf(w, "\t\tpthread_create(&th[%v+i], 0, thr, (void*)i);\n", len(calls))
			fmt.Fprintf(w, "\t\tif (rand()%%2)\n")
			fmt.Fprintf(w, "\t\t\tusleep(rand()%%10000);\n")
			fmt.Fprintf(w, "\t}\n")
		}
		fmt.Fprintf(w, "\tusleep(100000);\n")
		fmt.Fprintf(w, "}\n\n")
	}
}

func generateCalls(exec []byte) ([]string, int) {
	read := func() uintptr {
		if len(exec) < 8 {
			panic("exec program overflow")
		}
		v := *(*uint64)(unsafe.Pointer(&exec[0]))
		exec = exec[8:]
		return uintptr(v)
	}
	resultRef := func() string {
		arg := read()
		res := fmt.Sprintf("r[%v]", arg)
		if opDiv := read(); opDiv != 0 {
			res = fmt.Sprintf("%v/%v", res, opDiv)
		}
		if opAdd := read(); opAdd != 0 {
			res = fmt.Sprintf("%v+%v", res, opAdd)
		}
		return res
	}
	lastCall := 0
	seenCall := false
	var calls []string
	w := new(bytes.Buffer)
	newCall := func() {
		if seenCall {
			seenCall = false
			calls = append(calls, w.String())
			w = new(bytes.Buffer)
		}
	}
	n := 0
loop:
	for ; ; n++ {
		switch instr := read(); instr {
		case prog.ExecInstrEOF:
			break loop
		case prog.ExecInstrCopyin:
			newCall()
			addr := read()
			typ := read()
			size := read()
			switch typ {
			case prog.ExecArgConst:
				arg := read()
				bfOff := read()
				bfLen := read()
				if bfOff == 0 && bfLen == 0 {
					fmt.Fprintf(w, "\tNONFAILING(*(uint%v_t*)0x%x = (uint%v_t)0x%x);\n", size*8, addr, size*8, arg)
				} else {
					fmt.Fprintf(w, "\tNONFAILING(STORE_BY_BITMASK(uint%v_t, 0x%x, 0x%x, %v, %v));\n", size*8, addr, arg, bfOff, bfLen)
				}
			case prog.ExecArgResult:
				fmt.Fprintf(w, "\tNONFAILING(*(uint%v_t*)0x%x = %v);\n", size*8, addr, resultRef())
			case prog.ExecArgData:
				data := exec[:size]
				exec = exec[(size+7)/8*8:]
				var esc []byte
				for _, v := range data {
					hex := func(v byte) byte {
						if v < 10 {
							return '0' + v
						}
						return 'a' + v - 10
					}
					esc = append(esc, '\\', 'x', hex(v>>4), hex(v<<4>>4))
				}
				fmt.Fprintf(w, "\tNONFAILING(memcpy((void*)0x%x, \"%s\", %v));\n", addr, esc, size)
			default:
				panic("bad argument type")
			}
		case prog.ExecInstrCopyout:
			addr := read()
			size := read()
			fmt.Fprintf(w, "\tif (r[%v] != -1)\n", lastCall)
			fmt.Fprintf(w, "\t\tNONFAILING(r[%v] = *(uint%v_t*)0x%x);\n", n, size*8, addr)
		default:
			// Normal syscall.
			newCall()
			meta := sys.Calls[instr]
			fmt.Fprintf(w, "\tr[%v] = execute_syscall(__NR_%v", n, meta.CallName)
			nargs := read()
			for i := uintptr(0); i < nargs; i++ {
				typ := read()
				size := read()
				_ = size
				switch typ {
				case prog.ExecArgConst:
					fmt.Fprintf(w, ", 0x%xul", read())
					// Bitfields can't be args of a normal syscall, so just ignore them.
					read() // bit field offset
					read() // bit field length
				case prog.ExecArgResult:
					fmt.Fprintf(w, ", %v", resultRef())
				default:
					panic("unknown arg type")
				}
			}
			for i := nargs; i < 9; i++ {
				fmt.Fprintf(w, ", 0")
			}
			fmt.Fprintf(w, ");\n")
			lastCall = n
			seenCall = true
		}
	}
	newCall()
	return calls, n
}

func preprocessCommonHeader(opts Options, handled map[string]int) (string, error) {
	var defines []string
	switch opts.Sandbox {
	case "none":
		defines = append(defines, "SYZ_SANDBOX_NONE")
	case "setuid":
		defines = append(defines, "SYZ_SANDBOX_SETUID")
	case "namespace":
		defines = append(defines, "SYZ_SANDBOX_NAMESPACE")
	default:
		return "", fmt.Errorf("unknown sandbox mode: %v", opts.Sandbox)
	}
	if opts.Repeat {
		defines = append(defines, "SYZ_REPEAT")
	}
	for name, _ := range handled {
		defines = append(defines, "__NR_"+name)
	}
	// TODO: need to know target arch + do cross-compilation
	defines = append(defines, "__x86_64__")

	cmd := exec.Command("cpp", "-nostdinc", "-undef", "-fdirectives-only", "-dDI", "-E", "-P", "-")
	for _, def := range defines {
		cmd.Args = append(cmd.Args, "-D"+def)
	}
	cmd.Stdin = strings.NewReader(commonHeader)
	stderr := new(bytes.Buffer)
	stdout := new(bytes.Buffer)
	cmd.Stderr = stderr
	cmd.Stdout = stdout
	if err := cmd.Run(); len(stdout.Bytes()) == 0 {
		return "", fmt.Errorf("cpp failed: %v\n%v\n%v\n", err, stdout.String(), stderr.String())
	}
	remove := append(defines, []string{
		"__STDC__",
		"__STDC_VERSION__",
		"__STDC_HOSTED__",
		"__STDC_UTF_16__",
		"__STDC_UTF_32__",
	}...)
	out := stdout.String()
	for _, def := range remove {
		out = strings.Replace(out, "#define "+def+" 1\n", "", -1)
	}
	return out, nil
}

// Build builds a C/C++ program from source src and returns name of the resulting binary.
// lang can be "c" or "c++".
func Build(lang, src string) (string, error) {
	bin, err := ioutil.TempFile("", "syzkaller")
	if err != nil {
		return "", fmt.Errorf("failed to create temp file: %v", err)
	}
	bin.Close()
	out, err := exec.Command("gcc", "-x", lang, "-Wall", "-Werror", src, "-o", bin.Name(), "-pthread", "-static", "-O1", "-g").CombinedOutput()
	if err != nil {
		// Some distributions don't have static libraries.
		out, err = exec.Command("gcc", "-x", lang, "-Wall", "-Werror", src, "-o", bin.Name(), "-pthread", "-O1", "-g").CombinedOutput()
	}
	if err != nil {
		os.Remove(bin.Name())
		data, _ := ioutil.ReadFile(src)
		return "", fmt.Errorf("failed to build program:\n%s\n%s", data, out)
	}
	return bin.Name(), nil
}

// Format reformats C source using clang-format.
func Format(src []byte) ([]byte, error) {
	stdout, stderr := new(bytes.Buffer), new(bytes.Buffer)
	cmd := exec.Command("clang-format", "-assume-filename=/src.c", "-style", style)
	cmd.Stdin = bytes.NewReader(src)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return src, fmt.Errorf("failed to format source: %v\n%v", err, stderr.String())
	}
	return stdout.Bytes(), nil
}

// Something acceptable for kernel developers and email-friendly.
var style = `{
BasedOnStyle: LLVM,
IndentWidth: 2,
UseTab: Never,
BreakBeforeBraces: Linux,
IndentCaseLabels: false,
DerivePointerAlignment: false,
PointerAlignment: Left,
AlignTrailingComments: true,
AllowShortBlocksOnASingleLine: false,
AllowShortCaseLabelsOnASingleLine: false,
AllowShortFunctionsOnASingleLine: false,
AllowShortIfStatementsOnASingleLine: false,
AllowShortLoopsOnASingleLine: false,
ColumnLimit: 72,
}`
