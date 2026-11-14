// Command patch prepares a Kubernetes checkout for GOOS=js GOARCH=wasm:
//
//	go run . <kubernetes checkout>
//
// kube-apiserver, kube-controller-manager, kube-scheduler and kubectl then
// build and run in the browser. The command
//
//   - excludes unix-only files from js with build tags,
//   - copies _overlay/ (js stubs and helpers) into the tree,
//   - applies a few content patches,
//   - vendors mvdan.cc/sh (the terminal's shell) at MVDAN_SH_VERSION and
//   - copies tabernetes/ (the main package) to cmd/tabernetes.
//
// Every patch fails when its target is gone. A Kubernetes update shows exactly
// what needs attention.
package main

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

//go:embed all:_overlay all:tabernetes
var files embed.FS

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: go run . <kubernetes checkout>")
		os.Exit(2)
	}
	if err := os.Chdir(os.Args[1]); err != nil {
		fail("%v", err)
	}

	// Exclude unix-only files from js. The js variant comes from _overlay/ or
	// is a copy of the windows variant.
	for _, f := range []string{
		"vendor/github.com/coreos/go-systemd/v22/journal/journal_unix.go",
		"vendor/go.etcd.io/etcd/client/pkg/v3/fileutil/lock_flock.go",
		"vendor/go.etcd.io/etcd/client/pkg/v3/fileutil/lock_unix.go",
		"vendor/github.com/moby/term/term_unix.go",
		"vendor/github.com/moby/term/termios_unix.go",
		"vendor/github.com/moby/term/termios_nonbsd.go",
		"pkg/probe/dialer_others.go",
	} {
		buildTag(f, "(%s) && !js")
	}
	for unix, windows := range map[string]string{
		"vendor/github.com/coreos/go-systemd/v22/journal/journal_unix.go": "vendor/github.com/coreos/go-systemd/v22/journal/journal_windows.go",
		"staging/src/k8s.io/apiserver/pkg/server/options/serving_unix.go": "staging/src/k8s.io/apiserver/pkg/server/options/serving_windows.go",
		"staging/src/k8s.io/kubectl/pkg/util/umask.go":                    "staging/src/k8s.io/kubectl/pkg/util/umask_windows.go",
		"pkg/scheduler/backend/cache/debugger/signal.go":                  "pkg/scheduler/backend/cache/debugger/signal_windows.go",
		"staging/src/k8s.io/kubectl/pkg/util/term/resizeevents.go":        "staging/src/k8s.io/kubectl/pkg/util/term/resizeevents_windows.go",
	} {
		jsFromWindows(unix, windows)
	}
	// kubectl edit: no editor process in js. _overlay/ has a launcher that
	// runs the editor in-process (the vi of cmd/tabernetes).
	buildTag("staging/src/k8s.io/kubectl/pkg/cmd/util/editor/launcher_others.go", "(%s) && !js")

	// The plain file system helpers work in js as well.
	buildTag("pkg/util/filesystem/util_unix.go", "%s || js")

	copyTree("_overlay", ".")

	// mount-utils: move the unix syscalls into mount_helper_sys_{unix,js}.go.
	edit("staging/src/k8s.io/mount-utils/mount_helper_unix.go",
		"\t\terr = syscall.Access(path, syscall.F_OK)\n\t\tif err == nil {",
		"\t\tif accessExists(path) {",
		"\t\tuname := unix.Utsname{}\n\t\terr := unix.Uname(&uname)\n\t\tif err != nil {\n\t\t\treturn\n\t\t}\n\n\t\tend := bytes.IndexByte(uname.Release[:], 0)\n\t\tv := bytes.SplitN(uname.Release[:end]",
		"\t\trelease, ok := unameRelease()\n\t\tif !ok {\n\t\t\treturn\n\t\t}\n\n\t\tv := bytes.SplitN(release",
		"\t\"golang.org/x/sys/unix\"\n", "",
	)

	// kubectl: no SIGHUP/SIGQUIT in js, and no syscall.Exec for plugins.
	edit("staging/src/k8s.io/kubectl/pkg/util/interrupt/interrupt.go",
		"syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT",
		"syscall.SIGINT, syscall.SIGTERM",
	)
	edit("staging/src/k8s.io/kubectl/pkg/cmd/plugin.go",
		"return syscall.Exec(executablePath,",
		"return syscallExec(executablePath,",
	)
	dropUnusedImport("staging/src/k8s.io/kubectl/pkg/cmd/plugin.go", "syscall")
	// kubectl translations: the first i18n.T() call, in the package init of
	// kubectl/pkg/cmd/options, zips the .po and .mo files and parses them
	// (~0.1 s in Wasm). The terminal sets no LANG, so kubectl always speaks
	// English anyway. Without a loaded locale, i18n.T returns the English text.
	edit("staging/src/k8s.io/kubectl/pkg/util/i18n/i18n.go",
		"\t\treturn LoadTranslations(\"kubectl\", nil)\n",
		"\t\treturn nil\n",
	)

	// kube-controller-manager builds all its ~40 controllers, then starts the
	// informers and all controllers at once. In Wasm they share one thread
	// without preemption: the node IPAM controller gave the tab node its pod
	// CIDR only ~2.3 s after the start, and the tab node runs no pod before
	// it has one. Build and run that controller first, the others follow as
	// before.
	edit("cmd/kube-controller-manager/app/controllermanager.go",
		"\t\t// Prepare all controllers in advance.\n",
		`		// tabernetes: the node IPAM controller first (k8s/patch.go).
		if d := controllerDescriptors[names.NodeIpamController]; d != nil {
			first, err := BuildControllers(ctx, controllerContext, map[string]*ControllerDescriptor{d.Name(): d}, unsecuredMux, healthzHandler)
			if err != nil {
				return err
			}
			controllerContext.InformerFactory.Start(ctx.Done())
			for _, c := range first {
				go c.Run(ctx)
			}
			rest := make(map[string]*ControllerDescriptor, len(controllerDescriptors))
			for name, desc := range controllerDescriptors {
				if desc != d {
					rest[name] = desc
				}
			}
			controllerDescriptors = rest
		}

		// Prepare all controllers in advance.
`,
	)

	// RSA key generation is painfully slow in single-threaded wasm: use ECDSA
	// for self-signed certificates.
	edit("staging/src/k8s.io/client-go/util/cert/cert.go",
		"rsa.GenerateKey(cryptorand.Reader, 2048)",
		"ecdsa.GenerateKey(elliptic.P256(), cryptorand.Reader)",
		"\tif err := pem.Encode(&keyBuffer, &pem.Block{Type: keyutil.RSAPrivateKeyBlockType, Bytes: x509.MarshalPKCS1PrivateKey(priv)}); err != nil {",
		"\tprivDER, err := x509.MarshalECPrivateKey(priv)\n\tif err != nil {\n\t\treturn nil, nil, err\n\t}\n\tif err := pem.Encode(&keyBuffer, &pem.Block{Type: keyutil.ECPrivateKeyBlockType, Bytes: privDER}); err != nil {",
		"\t\"crypto/rsa\"\n",
		"\t\"crypto/ecdsa\"\n\t\"crypto/elliptic\"\n",
	)

	// wasm has no preemption: give the informer a chance to catch up instead of
	// busy-looping.
	edit("pkg/registry/flowcontrol/ensurer/strategy.go",
		"\t\tklog.V(5).InfoS(fmt.Sprintf(\"Something created the %T concurrently\", bootstrap), \"type\", configurationType, \"name\", name)\n\t}",
		"\t\tklog.V(5).InfoS(fmt.Sprintf(\"Something created the %T concurrently\", bootstrap), \"type\", configurationType, \"name\", name)\n\t\ttime.Sleep(100 * time.Millisecond)\n\t}",
	)
	addImport("pkg/registry/flowcontrol/ensurer/strategy.go", "time")

	// All components run in one process: make the signal handler setup
	// idempotent.
	edit("staging/src/k8s.io/apiserver/pkg/server/signal.go",
		"func SetupSignalContext() context.Context {\n\tclose(onlyOneSignalHandler) // panics when called twice\n",
		"func SetupSignalContext() context.Context {\n\tsignalOnce.Do(func() { signalCtx = setupSignalContext() })\n\treturn signalCtx\n}\n\nvar (\n\tsignalOnce sync.Once\n\tsignalCtx  context.Context\n)\n\nfunc setupSignalContext() context.Context {\n\tclose(onlyOneSignalHandler) // panics when called twice\n",
		"\t\"os/signal\"\n",
		"\t\"os/signal\"\n\t\"sync\"\n",
	)

	// js has no fsnotify. The certificate and CA file watchers retry every
	// minute and log an error each time. The periodic check before the watch
	// still reloads the files, so skip the watch quietly.
	for _, f := range []string{
		"staging/src/k8s.io/apiserver/pkg/server/dynamiccertificates/dynamic_cafile_content.go",
		"staging/src/k8s.io/apiserver/pkg/server/dynamiccertificates/dynamic_serving_content.go",
	} {
		edit(f,
			"\tw, err := fsnotify.NewWatcher()\n\tif err != nil {\n\t\treturn fmt.Errorf(\"error creating fsnotify watcher: %v\", err)\n\t}",
			"\tw, err := fsnotify.NewWatcher()\n\tif err != nil {\n\t\treturn nil // tabernetes: no fsnotify in js\n\t}",
		)
	}

	// The shell of the terminal. Its only other dependencies, golang.org/x/sys
	// and golang.org/x/term, are vendored by Kubernetes already.
	vendorModule("mvdan.cc/sh/v3", os.Getenv("MVDAN_SH_VERSION"),
		"expand", "fileutil", "internal", "interp", "pattern", "syntax")

	// A background job ("k port-forward ... &") runs with the context of its
	// command line, which ends with the line. The hook lets tabernetes give each
	// job its own context (tabernetes/jobs.go).
	edit("vendor/mvdan.cc/sh/v3/interp/runner.go",
		"\t\tgo func() {\n\t\t\tr2.Run(ctx, &st2)\n",
		"\t\tctx, done := BackgroundContext(ctx, &st2) // tabernetes\n\t\tgo func() {\n\t\t\tdefer done()\n\t\t\tr2.Run(ctx, &st2)\n",
	)
	write("vendor/mvdan.cc/sh/v3/interp/tabernetes.go", `package interp

import (
	"context"

	"mvdan.cc/sh/v3/syntax"
)

// BackgroundContext returns the context of a background job and the func
// called when the job ends. Added by tabernetes (k8s/patch.go).
var BackgroundContext = func(ctx context.Context, stmt *syntax.Stmt) (context.Context, func()) {
	return ctx, func() {}
}
`)

	// port-forward and proxy serve until the process ends. Here the process is
	// the browser tab, so stop them with the command's context (Ctrl-C,
	// kill %1). port-forward has RunPortForwardContext, the command does not
	// use it.
	edit("staging/src/k8s.io/kubectl/pkg/cmd/portforward/portforward.go",
		"cmdutil.CheckErr(opts.RunPortForward())", "cmdutil.CheckErr(opts.RunPortForwardContext(cmd.Context()))",
	)
	// The status line under the terminal links every forwarded port
	// (tabernetes/forwards.go).
	edit("staging/src/k8s.io/kubectl/pkg/cmd/portforward/portforward.go",
		"\treturn fw.ForwardPorts()\n}",
		`	stopped := make(chan struct{}) // tabernetes: report the ports while they listen
	defer close(stopped)
	go func() {
		select {
		case <-opts.ReadyChannel:
		case <-stopped:
			return
		}
		ports, err := fw.GetPorts()
		if err != nil {
			return
		}
		done := OnForward(opts.Namespace, opts.PodName, ports)
		<-stopped
		done()
	}()
	return fw.ForwardPorts()
}`,
	)
	write("staging/src/k8s.io/kubectl/pkg/cmd/portforward/tabernetes.go", `package portforward

import "k8s.io/client-go/tools/portforward"

// OnForward is called when the local ports listen and returns the func called
// when they stop. Added by tabernetes (k8s/patch.go).
var OnForward = func(namespace, pod string, ports []portforward.ForwardedPort) func() {
	return func() {}
}
`)
	edit("staging/src/k8s.io/kubectl/pkg/cmd/proxy/proxy.go",
		"import (\n", "import (\n\t\"context\"\n",
		"cmdutil.CheckErr(o.RunProxy())", "cmdutil.CheckErr(o.runProxyContext(cmd.Context()))",
		"func (o ProxyOptions) RunProxy() error {\n",
		"func (o ProxyOptions) RunProxy() error { return o.runProxyContext(context.Background()) }\n\nfunc (o ProxyOptions) runProxyContext(ctx context.Context) error {\n",
		"\treturn server.ServeOnListener(l)\n",
		"\tstop := context.AfterFunc(ctx, func() { l.Close() })\n\tdefer stop()\n\tif err := server.ServeOnListener(l); ctx.Err() == nil {\n\t\treturn err\n\t}\n\treturn nil\n",
	)

	// The tabernetes main package.
	if err := os.RemoveAll("cmd/tabernetes"); err != nil {
		fail("%v", err)
	}
	copyTree("tabernetes", "cmd/tabernetes")
}

// vendorModule adds packages of a module that Kubernetes does not use to the
// vendor directory, the way "go mod vendor" would: go.mod, go.sum,
// vendor/modules.txt and the sources without tests.
func vendorModule(path, version string, pkgs ...string) {
	if version == "" {
		fail("vendor %s: no version set", path)
	}
	cmd := exec.Command("go", "mod", "download", "-json", path+"@"+version)
	cmd.Dir = os.TempDir()
	cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=")
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		fail("vendor %s: go mod download: %v", path, err)
	}
	var mod struct{ Dir, GoMod, Sum, GoModSum string }
	if err := json.Unmarshal(out, &mod); err != nil {
		fail("vendor %s: %v", path, err)
	}
	goVersion := regexp.MustCompile(`(?m)^go (\S+)`).FindStringSubmatch(read(mod.GoMod))
	if goVersion == nil {
		fail("vendor %s: no go directive in go.mod", path)
	}

	modules := read("vendor/modules.txt")
	if strings.Contains(modules, "\n# "+path+" ") {
		fail("vendor %s: Kubernetes vendors it already, drop vendorModule", path)
	}
	entry := fmt.Sprintf("# %s %s\n## explicit; go %s\n", path, version, goVersion[1])
	for _, p := range pkgs {
		entry += path + "/" + p + "\n"
	}
	// modules.txt is sorted by module path.
	at := len(modules)
	for _, m := range regexp.MustCompile(`(?m)^# (\S+) `).FindAllStringSubmatchIndex(modules, -1) {
		if modules[m[2]:m[3]] > path {
			at = m[0]
			break
		}
	}
	write("vendor/modules.txt", modules[:at]+entry+modules[at:])
	gomod := read("go.mod")
	if !strings.Contains(gomod, "\nrequire (\n") {
		fail("go.mod: patch target not found: require block")
	}
	write("go.mod", strings.Replace(gomod, "\nrequire (\n", "\nrequire (\n\t"+path+" "+version+"\n", 1))
	write("go.sum", read("go.sum")+fmt.Sprintf("%s %s %s\n%s %s/go.mod %s\n", path, version, mod.Sum, path, version, mod.GoModSum))

	dst := filepath.Join("vendor", path)
	for _, p := range pkgs {
		entries, err := os.ReadDir(filepath.Join(mod.Dir, p))
		if err != nil {
			fail("vendor %s: %v", path, err)
		}
		must(os.MkdirAll(filepath.Join(dst, p), 0o755))
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
				continue
			}
			b, err := os.ReadFile(filepath.Join(mod.Dir, p, e.Name()))
			must(err)
			// "go mod vendor" drops files with an ignore build tag as well.
			if bytes.Contains(b, []byte("//go:build ignore")) {
				continue
			}
			must(os.WriteFile(filepath.Join(dst, p, e.Name()), b, 0o644))
		}
	}
	b, err := os.ReadFile(filepath.Join(mod.Dir, "LICENSE"))
	must(err)
	must(os.WriteFile(filepath.Join(dst, "LICENSE"), b, 0o644))
}

func must(err error) {
	if err != nil {
		fail("%v", err)
	}
}

func fail(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	fmt.Fprintln(os.Stderr, "patch: "+msg)
	if os.Getenv("GITHUB_ACTIONS") == "true" {
		// An annotation puts the failing patch at the top of the run and the PR.
		fmt.Printf("::error file=tabernetes/k8s/patch.go,title=Kubernetes patch failed::%s\n", msg)
	}
	os.Exit(1)
}

func read(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		fail("%v", err)
	}
	return string(b)
}

func write(path, s string) {
	if err := os.WriteFile(path, []byte(s), 0o644); err != nil {
		fail("%v", err)
	}
}

// edit replaces each old with its new in path. pairs is old, new, old, new, ...
func edit(path string, pairs ...string) {
	s := read(path)
	for i := 0; i < len(pairs); i += 2 {
		if !strings.Contains(s, pairs[i]) {
			fail("%s: patch target not found: %q", path, pairs[i])
		}
		s = strings.ReplaceAll(s, pairs[i], pairs[i+1])
	}
	write(path, s)
}

var (
	goBuild   = regexp.MustCompile(`(?m)^//go:build (.+)$`)
	plusBuild = regexp.MustCompile(`(?m)^// \+build .*\n`)
)

// buildTag rewrites the //go:build line of path with format, where %s is the
// old expression. Legacy // +build lines are dropped.
func buildTag(path, format string) {
	s := read(path)
	m := goBuild.FindStringSubmatchIndex(s)
	if m == nil {
		fail("%s: patch target not found: //go:build line", path)
	}
	expr := s[m[2]:m[3]]
	if strings.Contains(expr, "js") {
		fail("%s: already patched: %s", path, expr)
	}
	s = s[:m[2]] + fmt.Sprintf(format, expr) + s[m[3]:]
	write(path, plusBuild.ReplaceAllString(s, ""))
}

// jsFromWindows excludes unix from js and copies windows to a _js.go file
// with a js build tag.
func jsFromWindows(unix, windows string) {
	if !strings.Contains(read(unix), "&& !js") {
		buildTag(unix, "(%s) && !js")
	}
	s := plusBuild.ReplaceAllString(goBuild.ReplaceAllString(read(windows), ""), "")
	write(strings.TrimSuffix(windows, "_windows.go")+"_js.go", "//go:build js\n\n"+strings.TrimLeft(s, "\n"))
}

func dropUnusedImport(path, pkg string) {
	if regexp.MustCompile(`\b` + pkg + `\.[A-Z]`).MatchString(read(path)) {
		fail("%s: %s is still used", path, pkg)
	}
	edit(path, "\t\""+pkg+"\"\n", "")
}

func addImport(path, pkg string) {
	if !strings.Contains(read(path), "\t\""+pkg+"\"\n") {
		edit(path, "import (\n", "import (\n\t\""+pkg+"\"\n")
	}
}

// copyTree copies the embedded directory src into dst.
func copyTree(src, dst string) {
	err := fs.WalkDir(files, src, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := files.ReadFile(path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, strings.TrimPrefix(path, src+"/"))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, b, 0o644)
	})
	if err != nil {
		fail("%v", err)
	}
}
