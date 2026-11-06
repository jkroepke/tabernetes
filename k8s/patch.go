// Command patch prepares a Kubernetes checkout for GOOS=js GOARCH=wasm:
//
//	go run . <kubernetes checkout>
//
// kube-apiserver, kube-controller-manager, kube-scheduler and kubectl then
// build and run in the browser. The command
//
//   - excludes unix-only files from js with build tags,
//   - copies _overlay/ (js stubs and helpers) into the tree,
//   - applies a few content patches and
//   - copies kwasm/ (the main package) to cmd/kwasm.
//
// Every patch fails when its target is gone. A Kubernetes update shows exactly
// what needs attention.
package main

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

//go:embed all:_overlay all:kwasm
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

	// The kwasm main package.
	if err := os.RemoveAll("cmd/kwasm"); err != nil {
		fail("%v", err)
	}
	copyTree("kwasm", "cmd/kwasm")
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "patch: "+format+"\n", args...)
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
