package main

import (
	"cmp"
	"context"
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"
	"syscall/js"

	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	resourcehelper "k8s.io/kubernetes/pkg/api/v1/resource"
	"k8s.io/kubernetes/pkg/fieldpath"
	"k8s.io/kubernetes/third_party/forked/golang/expansion"
	"k8s.io/utils/ptr"
)

// The environment and the volumes of the containers of a tab pod, as the
// kubelet makes them: env with $(VAR), valueFrom and envFrom of ConfigMaps,
// Secrets, the downward API and the resources; the volumes configMap,
// secret, downwardAPI, projected (with the token of the service account)
// and emptyDir. The page puts the files of the volumes into the root file
// system of the VM (vmworker.js), and init binds them into the containers
// (vm/init/init.c). The files are read once at the start of the pod: a
// change of a ConfigMap does not reach a running pod.

// volDir is where the page puts the volume name in the VM.
func volDir(name string) string { return "/.tabernetes/v/" + name }

// supportedVolume tells whether the tab node has the volume.
func supportedVolume(v corev1.Volume) bool {
	s := v.VolumeSource
	return s.ConfigMap != nil || s.Secret != nil || s.DownwardAPI != nil || s.Projected != nil || s.EmptyDir != nil || s.PersistentVolumeClaim != nil
}

// unsupportedVolumes lists what the tab node does not have.
func unsupportedVolumes(pod *corev1.Pod) []string {
	var out []string
	for _, v := range pod.Spec.Volumes {
		if !supportedVolume(v) {
			out = append(out, "volume "+v.Name)
		}
	}
	return out
}

// podConfig is the env of each container and the files of the volumes.
type podConfig struct {
	env     map[string][]string
	volumes map[string]vmVolume
}

type vmVolume struct {
	files     []vmFile // nil for an emptyDir
	pv        string   // a PersistentVolume of the browser (provision.go)
	medium    string
	sizeLimit int64
	fsGroup   *int64
}

type vmFile struct {
	path string
	mode int32
	data []byte
}

// configJS is cfg for the page (vmworker.js): for each container its env
// and its command and args with $(VAR) expanded, and the volumes.
func (tp *tabPod) configJS(cfg *podConfig) js.Value {
	strs := func(l []string) []any {
		out := make([]any, len(l))
		for i, s := range l {
			out[i] = s
		}
		return out
	}
	ctrs := map[string]any{}
	tp.mu.Lock()
	for _, c := range tp.ctrs {
		e, ok := cfg.env[c.spec.Name]
		if !ok {
			continue
		}
		ctrs[c.spec.Name] = map[string]any{
			"env":     strs(e),
			"command": strs(expandAll(c.spec.Command, e)),
			"args":    strs(expandAll(c.spec.Args, e)),
		}
	}
	tp.mu.Unlock()
	vols := map[string]any{}
	for name, v := range cfg.volumes {
		m := map[string]any{"medium": v.medium}
		if v.pv != "" {
			m["pv"] = v.pv
		}
		if v.sizeLimit > 0 {
			m["sizeLimit"] = v.sizeLimit
		}
		if v.fsGroup != nil {
			m["fsGroup"] = int(*v.fsGroup)
		}
		if v.files != nil {
			files := make([]any, len(v.files))
			for i, f := range v.files {
				data := js.Global().Get("Uint8Array").New(len(f.data))
				js.CopyBytesToJS(data, f.data)
				files[i] = map[string]any{"path": f.path, "mode": int(f.mode), "data": data}
			}
			m["files"] = files
		}
		vols[name] = m
	}
	return js.ValueOf(map[string]any{"containers": ctrs, "volumes": vols})
}

// configError is a container whose config cannot be made, as the kubelet
// calls it CreateContainerConfigError.
type configError struct {
	ctr *tabCtr
	err error
}

func (e *configError) Error() string { return e.err.Error() }

// needsAPI tells whether the config of the pod needs the API server.
func (tp *tabPod) needsAPI() bool {
	for _, c := range tp.ctrs {
		if len(c.spec.EnvFrom) > 0 {
			return true
		}
		for _, e := range c.spec.Env {
			if e.ValueFrom != nil && (e.ValueFrom.ConfigMapKeyRef != nil || e.ValueFrom.SecretKeyRef != nil) {
				return true
			}
		}
	}
	for _, v := range tp.pod.Spec.Volumes {
		if v.ConfigMap != nil || v.Secret != nil || v.Projected != nil || v.PersistentVolumeClaim != nil {
			return true
		}
	}
	return false
}

// localConfig is the config of a pod that needs nothing of the API server;
// pending tells that it does, and the config comes later (apiConfig).
func (tp *tabPod) localConfig() (cfg *podConfig, pending bool) {
	if tp.needsAPI() {
		return nil, true
	}
	cfg, err := tp.apiConfig(context.Background())
	if err != nil {
		return nil, true
	}
	return cfg, false
}

// apiConfig makes the config of the pod.
func (tp *tabPod) apiConfig(ctx context.Context) (*podConfig, error) {
	cfg := &podConfig{env: map[string][]string{}, volumes: map[string]vmVolume{}}
	for _, c := range tp.ctrs {
		if c.kind == ctrEphemeral {
			continue
		}
		env, err := tp.containerEnv(ctx, c)
		if err != nil {
			return nil, &configError{c, err}
		}
		cfg.env[c.spec.Name] = env
	}
	for _, v := range tp.pod.Spec.Volumes {
		if !supportedVolume(v) {
			continue
		}
		vol, err := tp.volume(ctx, v)
		if err != nil {
			return nil, fmt.Errorf("MountVolume.SetUp failed for volume %q : %w", v.Name, err)
		}
		cfg.volumes[v.Name] = vol
	}
	return cfg, nil
}

// containerEnv is the environment of c, in the order of the kubelet:
// envFrom, then env, each $(VAR) expanded by those before.
func (tp *tabPod) containerEnv(ctx context.Context, c *tabCtr) ([]string, error) {
	vars := map[string]string{}
	var order []string
	set := func(k, v string) {
		if _, ok := vars[k]; !ok {
			order = append(order, k)
		}
		vars[k] = v
	}
	for _, from := range c.spec.EnvFrom {
		var data map[string]string
		switch {
		case from.ConfigMapRef != nil:
			cm, err := tp.n.cs.CoreV1().ConfigMaps(tp.namespace).Get(ctx, from.ConfigMapRef.Name, metav1.GetOptions{})
			if err != nil {
				if apierrors.IsNotFound(err) && ptr.Deref(from.ConfigMapRef.Optional, false) {
					continue
				}
				return nil, notFound("configmap", from.ConfigMapRef.Name, err)
			}
			data = cm.Data
		case from.SecretRef != nil:
			s, err := tp.n.cs.CoreV1().Secrets(tp.namespace).Get(ctx, from.SecretRef.Name, metav1.GetOptions{})
			if err != nil {
				if apierrors.IsNotFound(err) && ptr.Deref(from.SecretRef.Optional, false) {
					continue
				}
				return nil, notFound("secret", from.SecretRef.Name, err)
			}
			data = map[string]string{}
			for k, v := range s.Data {
				data[k] = string(v)
			}
		}
		keys := make([]string, 0, len(data))
		for k := range data {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			set(from.Prefix+k, data[k])
		}
	}
	mapping := func() func(string) string { return expansion.MappingFuncFor(vars, tp.serviceEnv()) }
	for _, e := range c.spec.Env {
		v, err := tp.envValue(ctx, c, e, mapping())
		if err != nil {
			return nil, err
		}
		if v != nil {
			set(e.Name, *v)
		}
	}
	env := make([]string, 0, len(order))
	for _, k := range order {
		env = append(env, k+"="+vars[k])
	}
	return env, nil
}

// serviceEnv would be the variables of the Services (enableServiceLinks).
// The tab node leaves them out: they come from the Services of the
// namespace when the pod starts, which takes a list for every pod.
func (tp *tabPod) serviceEnv() map[string]string { return nil }

func notFound(kind, name string, err error) error {
	if apierrors.IsNotFound(err) {
		return fmt.Errorf("%s %q not found", kind, name)
	}
	return err
}

// envValue is the value of e, or nil for an optional one that is missing.
func (tp *tabPod) envValue(ctx context.Context, c *tabCtr, e corev1.EnvVar, mapping func(string) string) (*string, error) {
	if e.ValueFrom == nil {
		v := expansion.Expand(e.Value, mapping)
		return &v, nil
	}
	f := e.ValueFrom
	switch {
	case f.FieldRef != nil:
		v, err := tp.fieldValue(f.FieldRef.FieldPath)
		return &v, err
	case f.ResourceFieldRef != nil:
		v, err := resourcehelper.ExtractResourceValueByContainerNameAndNodeAllocatable(f.ResourceFieldRef, tp.pod, cmp.Or(f.ResourceFieldRef.ContainerName, c.spec.Name), tp.allocatable())
		if f.ResourceFieldRef.ContainerName == "" {
			v, err = resourcehelper.ExtractResourceValueByContainerNameAndNodeAllocatable(f.ResourceFieldRef, tp.pod, c.spec.Name, tp.allocatable())
		}
		return &v, err
	case f.ConfigMapKeyRef != nil:
		r := f.ConfigMapKeyRef
		cm, err := tp.n.cs.CoreV1().ConfigMaps(tp.namespace).Get(ctx, r.Name, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) && ptr.Deref(r.Optional, false) {
				return nil, nil
			}
			return nil, notFound("configmap", r.Name, err)
		}
		v, ok := cm.Data[r.Key]
		if !ok {
			if b, ok := cm.BinaryData[r.Key]; ok {
				v := string(b)
				return &v, nil
			}
			if ptr.Deref(r.Optional, false) {
				return nil, nil
			}
			return nil, fmt.Errorf("couldn't find key %s in ConfigMap %s/%s", r.Key, tp.namespace, r.Name)
		}
		return &v, nil
	case f.SecretKeyRef != nil:
		r := f.SecretKeyRef
		s, err := tp.n.cs.CoreV1().Secrets(tp.namespace).Get(ctx, r.Name, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) && ptr.Deref(r.Optional, false) {
				return nil, nil
			}
			return nil, notFound("secret", r.Name, err)
		}
		b, ok := s.Data[r.Key]
		if !ok {
			if ptr.Deref(r.Optional, false) {
				return nil, nil
			}
			return nil, fmt.Errorf("couldn't find key %s in Secret %s/%s", r.Key, tp.namespace, r.Name)
		}
		v := string(b)
		return &v, nil
	}
	return nil, nil
}

// fieldValue is a field of the downward API.
func (tp *tabPod) fieldValue(p string) (string, error) {
	switch p {
	case "spec.nodeName":
		return tp.n.name, nil
	case "spec.serviceAccountName":
		return tp.pod.Spec.ServiceAccountName, nil
	case "status.podIP", "status.podIPs":
		return tp.ip, nil
	case "status.hostIP", "status.hostIPs":
		return tp.n.ip, nil
	}
	return fieldpath.ExtractFieldPathAsString(tp.pod, p)
}

// allocatable is what a container without limits gets in the downward API:
// the VM of a pod (vmworker.js) and the one CPU of the tab.
func (tp *tabPod) allocatable() corev1.ResourceList {
	return corev1.ResourceList{
		corev1.ResourceCPU:              resource.MustParse("1"),
		corev1.ResourceMemory:           resource.MustParse("256Mi"),
		corev1.ResourceEphemeralStorage: resource.MustParse("4Gi"),
	}
}

// expandAll expands $(VAR) in command and args with the env of the
// container.
func expandAll(l []string, env []string) []string {
	vars := map[string]string{}
	for _, e := range env {
		k, v, _ := strings.Cut(e, "=")
		vars[k] = v
	}
	m := expansion.MappingFuncFor(vars)
	out := make([]string, len(l))
	for i, s := range l {
		out[i] = expansion.Expand(s, m)
	}
	return out
}

// user is runAsUser[:runAsGroup] of the container or the pod, or "" for
// the user of the image.
func (tp *tabPod) user(c *tabCtr) string {
	var uid, gid *int64
	if sc := tp.pod.Spec.SecurityContext; sc != nil {
		uid, gid = sc.RunAsUser, sc.RunAsGroup
	}
	if sc := c.spec.SecurityContext; sc != nil {
		if sc.RunAsUser != nil {
			uid = sc.RunAsUser
		}
		if sc.RunAsGroup != nil {
			gid = sc.RunAsGroup
		}
	}
	switch {
	case uid == nil:
		return ""
	case gid == nil:
		return strconv.FormatInt(*uid, 10)
	}
	return strconv.FormatInt(*uid, 10) + ":" + strconv.FormatInt(*gid, 10)
}

// vmMounts are the volume mounts of c for init.
func (tp *tabPod) vmMounts(c *tabCtr) []any {
	vols := map[string]corev1.Volume{}
	for _, v := range tp.pod.Spec.Volumes {
		vols[v.Name] = v
	}
	var out []any
	for _, m := range c.spec.VolumeMounts {
		v, ok := vols[m.Name]
		if !ok || !supportedVolume(v) {
			continue
		}
		src := volDir(m.Name)
		sub := m.SubPath
		if m.SubPathExpr != "" {
			sub = m.SubPathExpr // expanded by the env: not done, rare
		}
		if sub != "" {
			src = path.Join(src, path.Clean("/"+sub))
		}
		// configMap, secret, downwardAPI and projected volumes are read-only
		ro := m.ReadOnly || v.EmptyDir == nil && v.PersistentVolumeClaim == nil || v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ReadOnly
		out = append(out, map[string]any{"src": src, "dst": m.MountPath, "ro": ro})
	}
	return out
}

// volume makes the files of v.
func (tp *tabPod) volume(ctx context.Context, v corev1.Volume) (vmVolume, error) {
	fsGroup := (*int64)(nil)
	if sc := tp.pod.Spec.SecurityContext; sc != nil {
		fsGroup = sc.FSGroup
	}
	s := v.VolumeSource
	switch {
	case s.PersistentVolumeClaim != nil:
		pv, err := tp.claimVolume(ctx, s.PersistentVolumeClaim.ClaimName)
		return vmVolume{pv: pv}, err
	case s.EmptyDir != nil:
		vol := vmVolume{medium: string(s.EmptyDir.Medium)}
		if s.EmptyDir.SizeLimit != nil {
			vol.sizeLimit = s.EmptyDir.SizeLimit.Value()
		}
		return vol, nil
	case s.ConfigMap != nil:
		files, err := tp.configMapFiles(ctx, s.ConfigMap.Name, s.ConfigMap.Items, s.ConfigMap.Optional, ptr.Deref(s.ConfigMap.DefaultMode, 0o644))
		return vmVolume{files: files, fsGroup: fsGroup}, err
	case s.Secret != nil:
		files, err := tp.secretFiles(ctx, s.Secret.SecretName, s.Secret.Items, s.Secret.Optional, ptr.Deref(s.Secret.DefaultMode, 0o644))
		return vmVolume{files: files, fsGroup: fsGroup}, err
	case s.DownwardAPI != nil:
		files, err := tp.downwardFiles(s.DownwardAPI.Items, ptr.Deref(s.DownwardAPI.DefaultMode, 0o644))
		return vmVolume{files: files, fsGroup: fsGroup}, err
	case s.Projected != nil:
		mode := ptr.Deref(s.Projected.DefaultMode, 0o644)
		files := []vmFile{}
		for _, p := range s.Projected.Sources {
			var more []vmFile
			var err error
			switch {
			case p.ConfigMap != nil:
				more, err = tp.configMapFiles(ctx, p.ConfigMap.Name, p.ConfigMap.Items, p.ConfigMap.Optional, mode)
			case p.Secret != nil:
				more, err = tp.secretFiles(ctx, p.Secret.Name, p.Secret.Items, p.Secret.Optional, mode)
			case p.DownwardAPI != nil:
				more, err = tp.downwardFiles(p.DownwardAPI.Items, mode)
			case p.ServiceAccountToken != nil:
				more, err = tp.tokenFile(ctx, p.ServiceAccountToken, mode)
			}
			if err != nil {
				return vmVolume{}, err
			}
			files = append(files, more...)
		}
		return vmVolume{files: files, fsGroup: fsGroup}, nil
	}
	return vmVolume{}, nil
}

// keyFiles maps the keys of a ConfigMap or Secret to files: all of them,
// or the items.
func keyFiles(kind, name string, data map[string][]byte, items []corev1.KeyToPath, mode int32, optional bool) ([]vmFile, error) {
	files := []vmFile{}
	if len(items) == 0 {
		keys := make([]string, 0, len(data))
		for k := range data {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			files = append(files, vmFile{path: k, mode: mode, data: data[k]})
		}
		return files, nil
	}
	for _, it := range items {
		d, ok := data[it.Key]
		if !ok {
			if optional {
				continue
			}
			return nil, fmt.Errorf("configmap references non-existent config key: %s", it.Key)
		}
		files = append(files, vmFile{path: path.Clean(it.Path), mode: ptr.Deref(it.Mode, mode), data: d})
	}
	_ = kind
	_ = name
	return files, nil
}

func (tp *tabPod) configMapFiles(ctx context.Context, name string, items []corev1.KeyToPath, optional *bool, mode int32) ([]vmFile, error) {
	cm, err := tp.n.cs.CoreV1().ConfigMaps(tp.namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) && ptr.Deref(optional, false) {
			return []vmFile{}, nil
		}
		return nil, notFound("configmap", name, err)
	}
	data := map[string][]byte{}
	for k, v := range cm.Data {
		data[k] = []byte(v)
	}
	for k, v := range cm.BinaryData {
		data[k] = v
	}
	return keyFiles("configmap", name, data, items, mode, ptr.Deref(optional, false))
}

func (tp *tabPod) secretFiles(ctx context.Context, name string, items []corev1.KeyToPath, optional *bool, mode int32) ([]vmFile, error) {
	s, err := tp.n.cs.CoreV1().Secrets(tp.namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) && ptr.Deref(optional, false) {
			return []vmFile{}, nil
		}
		return nil, notFound("secret", name, err)
	}
	return keyFiles("secret", name, s.Data, items, mode, ptr.Deref(optional, false))
}

func (tp *tabPod) downwardFiles(items []corev1.DownwardAPIVolumeFile, mode int32) ([]vmFile, error) {
	files := []vmFile{}
	for _, it := range items {
		var v string
		var err error
		switch {
		case it.FieldRef != nil:
			v, err = tp.fieldValue(it.FieldRef.FieldPath)
			if it.FieldRef.FieldPath == "metadata.labels" || it.FieldRef.FieldPath == "metadata.annotations" {
				v += "\n"
			}
		case it.ResourceFieldRef != nil:
			v, err = resourcehelper.ExtractResourceValueByContainerNameAndNodeAllocatable(it.ResourceFieldRef, tp.pod, it.ResourceFieldRef.ContainerName, tp.allocatable())
		}
		if err != nil {
			return nil, err
		}
		files = append(files, vmFile{path: path.Clean(it.Path), mode: ptr.Deref(it.Mode, mode), data: []byte(v)})
	}
	return files, nil
}

// tokenFile asks for a token of the service account of the pod, bound to
// the pod, as the kubelet does for kube-api-access-<id>.
func (tp *tabPod) tokenFile(ctx context.Context, p *corev1.ServiceAccountTokenProjection, mode int32) ([]vmFile, error) {
	sa := tp.pod.Spec.ServiceAccountName
	if sa == "" {
		sa = "default"
	}
	req := &authenticationv1.TokenRequest{Spec: authenticationv1.TokenRequestSpec{
		ExpirationSeconds: p.ExpirationSeconds,
		BoundObjectRef:    &authenticationv1.BoundObjectReference{Kind: "Pod", APIVersion: "v1", Name: tp.name, UID: tp.uid},
	}}
	if p.Audience != "" {
		req.Spec.Audiences = []string{p.Audience}
	}
	tr, err := tp.n.cs.CoreV1().ServiceAccounts(tp.namespace).CreateToken(ctx, sa, req, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to fetch token: %w", err)
	}
	// the kubelet writes the token readable by the owner only, unless the
	// pod has an fsGroup
	return []vmFile{{path: path.Clean(p.Path), mode: mode, data: []byte(tr.Status.Token)}}, nil
}
