// crt-stress is a tiny static workload used inside test containers.
//
// It has no dependencies beyond the Go runtime so it can live in a
// FROM-scratch image. Flags select what it does; with no flags it exits 0.
package main

import (
	"flag"
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"
)

func main() {
	mem := flag.Int("mem", 0, "allocate and touch this many MiB, then hold")
	hold := flag.Duration("hold", 2*time.Second, "how long to hold allocated memory")
	cpu := flag.Duration("cpu", 0, "burn CPU on all cores for this long")
	threads := flag.Int("threads", runtime.NumCPU(), "number of CPU burner goroutines")
	pid := flag.Bool("pid", false, "print own pid")
	hostname := flag.Bool("hostname", false, "print hostname")
	write := flag.String("write", "", "write a marker file at this path")
	remove := flag.String("rm", "", "remove this path")
	read := flag.String("read", "", "print contents of this file")
	ls := flag.String("ls", "", "list this directory")
	forks := flag.Int("fork", 0, "spawn this many child processes that sleep")
	sleep := flag.Duration("sleep", 0, "sleep this long")
	env := flag.String("env", "", "print this environment variable")
	cwd := flag.Bool("cwd", false, "print working directory")
	exit := flag.Int("exit", 0, "exit with this code")
	echo := flag.String("echo", "", "print this string")
	flag.Parse()

	if *echo != "" {
		fmt.Println(*echo)
	}
	if *pid {
		fmt.Println(os.Getpid())
	}
	if *hostname {
		h, _ := os.Hostname()
		fmt.Println(h)
	}
	if *cwd {
		d, _ := os.Getwd()
		fmt.Println(d)
	}
	if *env != "" {
		fmt.Println(os.Getenv(*env))
	}
	if *write != "" {
		if err := os.WriteFile(*write, []byte("marker\n"), 0o644); err != nil {
			fail(err)
		}
	}
	if *remove != "" {
		if err := os.RemoveAll(*remove); err != nil {
			fail(err)
		}
	}
	if *read != "" {
		b, err := os.ReadFile(*read)
		if err != nil {
			fail(err)
		}
		os.Stdout.Write(b)
	}
	if *ls != "" {
		ents, err := os.ReadDir(*ls)
		if err != nil {
			fail(err)
		}
		names := make([]string, 0, len(ents))
		for _, e := range ents {
			names = append(names, e.Name())
		}
		fmt.Println(strings.Join(names, " "))
	}
	if *forks > 0 {
		self, _ := os.Executable()
		var procs []*os.Process
		for i := 0; i < *forks; i++ {
			p, err := os.StartProcess(self, []string{self, "-sleep", "10s"}, &os.ProcAttr{})
			if err != nil {
				fmt.Fprintf(os.Stderr, "fork %d failed: %v\n", i, err)
				os.Exit(3)
			}
			procs = append(procs, p)
		}
		fmt.Printf("forked %d\n", len(procs))
		for _, p := range procs {
			p.Kill()
			p.Wait()
		}
	}
	if *mem > 0 {
		blocks := make([][]byte, 0, *mem)
		for i := 0; i < *mem; i++ {
			b := make([]byte, 1<<20)
			for j := 0; j < len(b); j += 4096 {
				b[j] = byte(i)
			}
			blocks = append(blocks, b)
		}
		fmt.Printf("allocated %d MiB\n", len(blocks))
		time.Sleep(*hold)
		runtime.KeepAlive(blocks)
	}
	if *cpu > 0 {
		deadline := time.Now().Add(*cpu)
		done := make(chan struct{})
		for i := 0; i < *threads; i++ {
			go func() {
				x := uint64(1)
				for time.Now().Before(deadline) {
					for k := 0; k < 1_000_000; k++ {
						x = x*6364136223846793005 + 1442695040888963407
					}
				}
				_ = x
				done <- struct{}{}
			}()
		}
		for i := 0; i < *threads; i++ {
			<-done
		}
		fmt.Printf("burned %s on %d threads\n", *cpu, *threads)
	}
	if *sleep > 0 {
		time.Sleep(*sleep)
	}
	os.Exit(*exit)
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "crt-stress:", err)
	os.Exit(2)
}
