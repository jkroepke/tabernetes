package main

// containerd calls runc through the agent (as tabernetes-runc) when the node runs
// without --privileged. The wrapper adapts the call and executes the real runc.

import (
	"bytes"
	"encoding/json"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

const runcWrapper = "/usr/local/bin/tabernetes-runc"

func runRuncWrapper(args []string) {
	out := make([]string, 0, len(args)+2)
	done := false
	for i, a := range args {
		out = append(out, a)
		if done || (a != "create" && a != "run") {
			continue
		}
		done = true
		if seccompFiltered() {
			// Docker's default seccomp profile blocks keyctl and pivot_root.
			out = append(out, "--no-new-keyring", "--no-pivot")
		}
		if runningInUserNS() {
			// Docker mounts the container's /sys read-only, so in a user
			// namespace the kernel allows only read-only sysfs mounts.
			if err := readOnlySysfs(bundleDir(args[i+1:])); err != nil {
				log.Printf("tabernetes-runc: %v", err)
			}
		}
	}
	runc, err := exec.LookPath("runc")
	if err != nil {
		log.Fatal(err)
	}
	log.Fatal(syscall.Exec(runc, append([]string{"runc"}, out...), os.Environ()))
}

// bundleDir finds the bundle in the arguments after "create" or "run".
func bundleDir(args []string) string {
	for i, a := range args {
		switch {
		case (a == "--bundle" || a == "-b") && i+1 < len(args):
			return args[i+1]
		case strings.HasPrefix(a, "--bundle="):
			return strings.TrimPrefix(a, "--bundle=")
		}
	}
	return "."
}

// readOnlySysfs adds "ro" to the sysfs mounts of the bundle's config.json.
func readOnlySysfs(bundle string) error {
	path := filepath.Join(bundle, "config.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber() // keep large numbers like memory limits exact
	var spec map[string]any
	if err := dec.Decode(&spec); err != nil {
		return err
	}
	mounts, _ := spec["mounts"].([]any)
	changed := false
	for _, m := range mounts {
		mount, _ := m.(map[string]any)
		if mount["type"] != "sysfs" {
			continue
		}
		opts, _ := mount["options"].([]any)
		ro := false
		for i, o := range opts {
			switch o {
			case "ro":
				ro = true
			case "rw":
				opts[i] = "ro"
				ro = true
			}
		}
		if !ro {
			opts = append(opts, "ro")
		}
		mount["options"] = opts
		changed = true
	}
	if !changed {
		return nil
	}
	data, err = json.Marshal(spec)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// runningInUserNS reports whether this process runs in a user namespace
// other than the initial one.
func runningInUserNS() bool {
	m, err := os.ReadFile("/proc/self/uid_map")
	if err != nil {
		return false
	}
	f := strings.Fields(string(m))
	return len(f) != 3 || f[0] != "0" || f[1] != "0" || f[2] != "4294967295"
}
