package main

import (
	"bytes"
	"flag"
	"fmt"
	"html/template"
	"io/ioutil"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"reflect"
	"sort"
	"sync"
	"syscall"
	"time"

	fsnotify "gopkg.in/fsnotify.v1"
)

var (
	// flags
	interval time.Duration
	execute  string
	tmplPath string
	dest     string
	debug    bool

	wg sync.WaitGroup
	mu sync.Mutex
)

func usage() {
	fmt.Printf(`Usage: dns-gen [options] hostname [hostname...]

Render template or execute commands based based on DNS updates

Options:
`)
	flag.PrintDefaults()

	fmt.Printf(`
Arguments:
  hostname: (required) One or more hostnames to watch for updates
`)
}

func parseFlags() {
	flag.DurationVar(&interval, "inter", 5*time.Second, "interval for DNS queries")
	flag.StringVar(&execute, "exec", "", "command to execute when a change is detected")
	flag.StringVar(&tmplPath, "tmpl", "", "if not empty, render this template to [dest | stdout]")
	flag.StringVar(&dest, "dest", "", "if tmpl is provided, it will be rendered to dest")
	flag.BoolVar(&debug, "debug", false, "enable debug logging")
	flag.Usage = usage
	flag.Parse()
}

var Funcs = template.FuncMap{
	"lookupHost": safeLookup,
	"add":        add,
	"addf":       addf,
	"mul":        mul,
	"mulf":       mulf,
	"div":        div,
	"divf":       divf,
}

func add(i, j int) int {
	return i + j
}

func addf(i, j float64) float64 {
	return i + j
}

func mul(i, j int) int {
	return i * j
}

func mulf(i, j float64) float64 {
	return i * j
}

func div(i, j int) int {
	return i / j
}

func divf(i, j float64) float64 {
	return i / j
}

func safeLookup(hn string) []string {
	ips, _ := lookup(hn)
	return ips
}

func newTemplate(name string) *template.Template {
	return template.New(name).Funcs(Funcs)
}

// Executes a template located at path with the specified data
func execTemplateFile(path string, data interface{}) ([]byte, error) {
	tmpl, err := newTemplate(filepath.Base(path)).ParseFiles(path)
	if err != nil {
		return nil, err
	}
	return execTemplate(tmpl, data)
}

// Helper for execTemplateFile and execTemplateString - actually executes the template
func execTemplate(tmpl *template.Template, data interface{}) ([]byte, error) {
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func sameLength(a, b []string) bool {
	return len(a) == len(b)
}

func sameContents(a, b []string) bool {
	return reflect.DeepEqual(a, b)
}

func equivalent(a, b []string) bool {
	return sameLength(a, b) && sameContents(a, b)
}

func lookup(hostname string) ([]string, error) {
	addresses, err := net.LookupHost(hostname)
	if err != nil {
		return nil, err
	}
	sort.Strings(addresses)
	return addresses, nil
}

func react() {
	mu.Lock()
	defer mu.Unlock()

	if tmplPath != "" {
		start := time.Now()
		content, err := execTemplateFile(tmplPath, nil)
		if err != nil {
			log.Printf("[ERROR] failed to execute template: %v\n", err)
		} else if debug {
			log.Printf("[DEBUG] template [%s] generated in %v\n", tmplPath, time.Since(start))
		}
		if err := writeFile(content); err != nil {
			log.Printf("[ERROR] failed to write output file: %v\n", err)
		}
	}
	if execute != "" {
		if err := runCmd(execute); err != nil {
			log.Printf("[ERROR] failed to execute command: %v\n", err)
		}
	}
}

func runCmd(cs string) error {
	start := time.Now()
	if debug {
		log.Printf("[DEBUG] running command [%v]...", cs)
	}
	cmd := exec.Command("/bin/sh", "-c", cs)
	out, err := cmd.CombinedOutput()
	log.Printf("[INFO] ran command [%v] in %v.\n", cs, time.Since(start))
	if err != nil {
		log.Printf("[ERROR] command [%v] failed with output: %s\n", cs, out)
	} else if debug {
		log.Printf("[DEBUG] output: %s\n", out)
	}
	return err
}

func writeFile(content []byte) error {
	if dest == "" {
		os.Stdout.Write(content)
		return nil
	}

	start := time.Now()
	// write to a temp file first so we can copy it into place with a single atomic operation
	tmp, err := ioutil.TempFile("", fmt.Sprintf("dns-gen-%d", time.Now().UnixNano()))
	defer func() {
		tmp.Close()
		os.Remove(tmp.Name())
	}()
	if err != nil {
		return fmt.Errorf("error creating temp file: %v", err)
	}

	if _, err := tmp.Write(content); err != nil {
		return fmt.Errorf("error writing temp file: %v", err)
	}

	var oldContent []byte
	if fi, err := os.Stat(dest); err == nil {
		// set permissions and ownership on new file
		if err := tmp.Chmod(fi.Mode()); err != nil {
			return fmt.Errorf("error setting file permissions: %v", err)
		}
		if err := tmp.Chown(int(fi.Sys().(*syscall.Stat_t).Uid), int(fi.Sys().(*syscall.Stat_t).Gid)); err != nil {
			return fmt.Errorf("error changing file owner: %v", err)
		}
		if oldContent, err = ioutil.ReadFile(dest); err != nil {
			return fmt.Errorf("error comparing old version: %v", err)
		}
	}

	if bytes.Compare(oldContent, content) != 0 {
		if err = os.Rename(tmp.Name(), dest); err != nil {
			return fmt.Errorf("error creating output file: %v", err)
		}
		log.Printf("output file [%s] created in %v\n", dest, time.Since(start))
	}

	return nil
}

func monitor(hostname string, interval time.Duration) {
	defer wg.Done()

	var knownAddresses []string
	sigCh := newSigChan()
	ticker := time.NewTicker(interval)
	first := time.After(0)

	refresh := func() error {
		start := time.Now()
		addresses, err := lookup(hostname)
		if err != nil {
			if de, ok := err.(*net.DNSError); ok && de.Temporary() {
				log.Printf("temporary error resolving hostname: %v. will retry...\n")
				return err
			} else {
				log.Printf("error resolving hostname: %v\n", err)
			}
		}
		if debug {
			log.Printf("[DEBUG] lookup [%s] => %v in %v\n", hostname, addresses, time.Since(start))
		}
		if !equivalent(knownAddresses, addresses) {
			log.Printf("[CHANGE] %s %s -> %s", hostname, knownAddresses, addresses)
			knownAddresses = addresses
			react()
		}
		return nil
	}

	for {
		select {
		case <-first:
			refresh()
		case <-ticker.C:
			refresh()
		case sig := <-sigCh:
			if sig == syscall.SIGTERM || sig == syscall.SIGINT {
				ticker.Stop()
				return
			}
		}
	}
}

// watch for changes to the template file and regenerate
func watchTemplate() {
	defer wg.Done()
	if tmplPath == "" {
		return
	}

	watch, err := fsnotify.NewWatcher()
	if err != nil {
		log.Fatalf("error watching template file for changes: %v\n", err)
	}
	go func() {
		defer watch.Close()
		sigCh := newSigChan()
		for {
			select {
			case ev := <-watch.Events:
				if ev.Name == tmplPath && (ev.Op == fsnotify.Write || ev.Op == fsnotify.Create) {
					log.Printf("[CHANGE] template changed: %#v\n", ev)
					react()
				}
			case err := <-watch.Errors:
				log.Printf("watch error: %v", err)
			case <-sigCh:
				return
			}
		}
	}()

	err = watch.Add(filepath.Dir(tmplPath))
	if err != nil {
		log.Fatalf("error watching template file for changes: %v\n", err)
	}
}

func newSigChan() <-chan os.Signal {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGHUP, syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTERM)
	return ch
}

// watch for signals
// trigger refresh on sighup
// exit on sigterm
func watchSignals() {
	defer wg.Done()
	sigCh := newSigChan()
	for sig := range sigCh {
		if sig == syscall.SIGTERM || sig == syscall.SIGINT {
			return
		} else if sig == syscall.SIGHUP {
			log.Printf("[CHANGE] caught SIGHUP\n")
			react()
		} else {
			log.Printf("signal caught: %v\n", sig)
		}
	}
}

func monitorHosts(interval time.Duration, execute string) {
	log.Printf("[INFO] Monitoring %d hosts every %v: %+v", flag.NArg(), interval, flag.Args())
	for _, hostname := range flag.Args() {
		wg.Add(1)
		go monitor(hostname, interval)
	}
}

func noHostsProvided() bool {
	return flag.NArg() == 0
}

func templateMissing() bool {
	if tmplPath != "" {
		if _, err := os.Stat(tmplPath); os.IsNotExist(err) {
			return true
		}
	}
	return false
}

func main() {
	parseFlags()
	if noHostsProvided() {
		log.Printf("No hostnames provided")
		flag.Usage()
		os.Exit(1)
	}

	if templateMissing() {
		log.Fatalf("temlpate file not found: %v\n", tmplPath)
	}

	monitorHosts(interval, execute)
	wg.Add(2)
	go watchTemplate()
	go watchSignals()
	wg.Wait()
}
