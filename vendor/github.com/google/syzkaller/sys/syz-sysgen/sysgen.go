// Copyright 2015/2016 syzkaller project authors. All rights reserved.
// Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.

package main

import (
	"bytes"
	"flag"
	"fmt"
	"go/format"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"sort"
	"strings"
	"sync"
	"text/template"

	"github.com/google/syzkaller/pkg/ast"
	"github.com/google/syzkaller/pkg/compiler"
	"github.com/google/syzkaller/pkg/hash"
	"github.com/google/syzkaller/pkg/osutil"
	"github.com/google/syzkaller/pkg/serializer"
	"github.com/google/syzkaller/prog"
	"github.com/google/syzkaller/sys/targets"
)

var (
	flagMemProfile = flag.String("memprofile", "", "write a memory profile to the file")
)

func main() {
	flag.Parse()

	for OS, archs := range targets.List {
		top := ast.ParseGlob(filepath.Join("sys", OS, "*.txt"), nil)
		if top == nil {
			os.Exit(1)
		}
		osutil.MkdirAll(filepath.Join("sys", OS, "gen"))

		type Job struct {
			Target      *targets.Target
			OK          bool
			Errors      []string
			Unsupported map[string]bool
			ArchData    []byte
		}
		var jobs []*Job
		for _, target := range archs {
			jobs = append(jobs, &Job{
				Target: target,
			})
		}
		sort.Slice(jobs, func(i, j int) bool {
			return jobs[i].Target.Arch < jobs[j].Target.Arch
		})
		var wg sync.WaitGroup
		wg.Add(len(jobs))

		for _, job := range jobs {
			job := job
			go func() {
				defer wg.Done()
				eh := func(pos ast.Pos, msg string) {
					job.Errors = append(job.Errors, fmt.Sprintf("%v: %v\n", pos, msg))
				}
				consts := compiler.DeserializeConstsGlob(filepath.Join("sys", OS, "*_"+job.Target.Arch+".const"), eh)
				if consts == nil {
					return
				}
				prog := compiler.Compile(top, consts, job.Target, eh)
				if prog == nil {
					return
				}
				job.Unsupported = prog.Unsupported

				sysFile := filepath.Join("sys", OS, "gen", job.Target.Arch+".go")
				out := new(bytes.Buffer)
				generate(job.Target, prog, consts, out)
				rev := hash.String(out.Bytes())
				fmt.Fprintf(out, "const revision_%v = %q\n", job.Target.Arch, rev)
				writeSource(sysFile, out.Bytes())

				job.ArchData = generateExecutorSyscalls(job.Target, prog.Syscalls, rev)
				job.OK = true
			}()
		}
		wg.Wait()

		var syscallArchs [][]byte
		unsupported := make(map[string]int)
		for _, job := range jobs {
			fmt.Printf("generating %v/%v...\n", job.Target.OS, job.Target.Arch)
			for _, msg := range job.Errors {
				fmt.Print(msg)
			}
			if !job.OK {
				os.Exit(1)
			}
			syscallArchs = append(syscallArchs, job.ArchData)
			for u := range job.Unsupported {
				unsupported[u]++
			}
			fmt.Printf("\n")
		}

		for what, count := range unsupported {
			if count == len(jobs) {
				failf("%v is unsupported on all arches (typo?)", what)
			}
		}

		writeExecutorSyscalls(OS, syscallArchs)
	}

	if *flagMemProfile != "" {
		f, err := os.Create(*flagMemProfile)
		if err != nil {
			failf("could not create memory profile: ", err)
		}
		runtime.GC() // get up-to-date statistics
		if err := pprof.WriteHeapProfile(f); err != nil {
			failf("could not write memory profile: ", err)
		}
		f.Close()
	}
}

func generate(target *targets.Target, prg *compiler.Prog, consts map[string]uint64, out io.Writer) {
	fmt.Fprintf(out, "// AUTOGENERATED FILE\n\n")
	fmt.Fprintf(out, "package gen\n\n")
	fmt.Fprintf(out, "import . \"github.com/google/syzkaller/prog\"\n\n")

	fmt.Fprintf(out, "var Target_%v = &Target{"+
		"OS: %q, Arch: %q, Revision: revision_%v, PtrSize: %v, "+
		"PageSize: %v, NumPages: %v, DataOffset: %v, Syscalls: syscalls_%v, "+
		"Resources: resources_%v, Structs: structDescs_%v, Consts: consts_%v}\n\n",
		target.Arch, target.OS, target.Arch, target.Arch, target.PtrSize,
		target.PageSize, target.NumPages, target.DataOffset,
		target.Arch, target.Arch, target.Arch, target.Arch)

	fmt.Fprintf(out, "var resources_%v = ", target.Arch)
	serializer.Write(out, prg.Resources)
	fmt.Fprintf(out, "\n\n")

	fmt.Fprintf(out, "var structDescs_%v = ", target.Arch)
	serializer.Write(out, prg.StructDescs)
	fmt.Fprintf(out, "\n\n")

	fmt.Fprintf(out, "var syscalls_%v = ", target.Arch)
	serializer.Write(out, prg.Syscalls)
	fmt.Fprintf(out, "\n\n")

	constArr := make([]prog.ConstValue, 0, len(consts))
	for name, val := range consts {
		constArr = append(constArr, prog.ConstValue{Name: name, Value: val})
	}
	sort.Slice(constArr, func(i, j int) bool {
		return constArr[i].Name < constArr[j].Name
	})
	fmt.Fprintf(out, "var consts_%v = ", target.Arch)
	serializer.Write(out, constArr)
	fmt.Fprintf(out, "\n\n")
}

func generateExecutorSyscalls(target *targets.Target, syscalls []*prog.Syscall, rev string) []byte {
	type SyscallData struct {
		Name     string
		CallName string
		NR       int32
		NeedCall bool
	}
	type ArchData struct {
		Revision   string
		GOARCH     string
		CARCH      []string
		PageSize   uint64
		NumPages   uint64
		DataOffset uint64
		Calls      []SyscallData
	}
	data := ArchData{
		Revision:   rev,
		GOARCH:     target.Arch,
		CARCH:      target.CArch,
		PageSize:   target.PageSize,
		NumPages:   target.NumPages,
		DataOffset: target.DataOffset,
	}
	for _, c := range syscalls {
		data.Calls = append(data.Calls, SyscallData{
			Name:     c.Name,
			CallName: c.CallName,
			NR:       int32(c.NR),
			NeedCall: !target.SyscallNumbers || strings.HasPrefix(c.CallName, "syz_"),
		})
	}
	sort.Slice(data.Calls, func(i, j int) bool {
		return data.Calls[i].Name < data.Calls[j].Name
	})
	buf := new(bytes.Buffer)
	if err := archTempl.Execute(buf, data); err != nil {
		failf("failed to execute arch template: %v", err)
	}
	return buf.Bytes()
}

func writeExecutorSyscalls(OS string, archs [][]byte) {
	buf := new(bytes.Buffer)
	buf.WriteString("// AUTOGENERATED FILE\n\n")
	for _, arch := range archs {
		buf.Write(arch)
	}
	writeFile(filepath.Join("executor", fmt.Sprintf("syscalls_%v.h", OS)), buf.Bytes())
}

func writeSource(file string, data []byte) {
	src, err := format.Source(data)
	if err != nil {
		fmt.Printf("%s\n", data)
		failf("failed to format output: %v", err)
	}
	if oldSrc, err := ioutil.ReadFile(file); err == nil && bytes.Equal(src, oldSrc) {
		return
	}
	writeFile(file, src)
}

func writeFile(file string, data []byte) {
	outf, err := os.Create(file)
	if err != nil {
		failf("failed to create output file: %v", err)
	}
	defer outf.Close()
	outf.Write(data)
}

func failf(msg string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, msg+"\n", args...)
	os.Exit(1)
}

var archTempl = template.Must(template.New("").Parse(`
#if {{range $cdef := $.CARCH}}defined({{$cdef}}) || {{end}}0
#define GOARCH "{{.GOARCH}}"
#define SYZ_REVISION "{{.Revision}}"
#define SYZ_PAGE_SIZE {{.PageSize}}
#define SYZ_NUM_PAGES {{.NumPages}}
#define SYZ_DATA_OFFSET {{.DataOffset}}
unsigned syscall_count = {{len $.Calls}};
call_t syscalls[] = {
{{range $c := $.Calls}}	{"{{$c.Name}}", {{$c.NR}}{{if $c.NeedCall}}, (syscall_t){{$c.CallName}}{{end}}},
{{end}}
};
#endif
`))
