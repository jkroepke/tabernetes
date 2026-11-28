package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"syscall/js"
	"time"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/workqueue"
)

// Persistent volumes in the browser. The StorageClass standard
// (addons/storage.yaml, the default class) belongs to this provisioner. It
// works like an external provisioner of a CSI driver or the local-path
// provisioner of kind: once the scheduler chose a node for the first pod of
// a claim (WaitForFirstConsumer), it creates a PersistentVolume of the type
// local, and the PV controller of kube-controller-manager binds the two. A
// released volume of the reclaim policy Delete goes, with its data.
//
// The data is in the origin private file system (OPFS) of the browser,
// in tabernetes/volumes/<PV name>, so every tab of this browser has it and it
// survives a reload. The PV has the node affinity kubernetes.io/os=browser:
// the nodes from join cannot reach it. The pod worker loads the files at the
// start and writes back what changed every few seconds and when the pod
// stops (web/src/vmworker.js).

const (
	provisionerName = "tabernetes.dev/opfs"
	volumesPath     = "/var/lib/tabernetes/volumes/" // what the PV shows; the data is in OPFS
	// annotations of the PV controller and the scheduler
	annProvisioner   = "volume.kubernetes.io/storage-provisioner"
	annSelectedNode  = "volume.kubernetes.io/selected-node"
	annProvisionedBy = "pv.kubernetes.io/provisioned-by"
)

type provisioner struct {
	cs      kubernetes.Interface
	classes cache.Indexer
	claims  cache.Indexer
	pvs     cache.Indexer
	queue   workqueue.TypedRateLimitingInterface[string]
}

// runProvisioner runs the provisioner of the class standard.
func runProvisioner(ctx context.Context) error {
	cfg, err := clientcmd.BuildConfigFromFlags("", p("admin.kubeconfig"))
	if err != nil {
		return err
	}
	cfg.UserAgent = "tabernetes/provisioner"
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return err
	}
	f := informers.NewSharedInformerFactory(cs, 0)
	sc := f.Storage().V1().StorageClasses().Informer()
	ci := f.Core().V1().PersistentVolumeClaims().Informer()
	pi := f.Core().V1().PersistentVolumes().Informer()
	pr := &provisioner{cs: cs, classes: sc.GetIndexer(), claims: ci.GetIndexer(), pvs: pi.GetIndexer(),
		queue: workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[string]())}
	add := func(prefix string) cache.ResourceEventHandlerFuncs {
		key := func(o any) {
			if k, err := cache.DeletionHandlingMetaNamespaceKeyFunc(o); err == nil {
				pr.queue.Add(prefix + k)
			}
		}
		return cache.ResourceEventHandlerFuncs{AddFunc: key, UpdateFunc: func(_, o any) { key(o) }}
	}
	if _, err := ci.AddEventHandler(add("pvc/")); err != nil {
		return err
	}
	if _, err := pi.AddEventHandler(add("pv/")); err != nil {
		return err
	}
	// a claim that came before its class waits for it
	requeue := func(any) {
		for _, k := range pr.claims.ListKeys() {
			pr.queue.Add("pvc/" + k)
		}
	}
	if _, err := sc.AddEventHandler(cache.ResourceEventHandlerFuncs{AddFunc: requeue, UpdateFunc: func(_, o any) { requeue(o) }}); err != nil {
		return err
	}
	f.Start(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), sc.HasSynced, ci.HasSynced, pi.HasSynced) {
		return ctx.Err()
	}
	go func() {
		<-ctx.Done()
		pr.queue.ShutDown()
	}()
	go pr.sweep(ctx)
	for {
		k, quit := pr.queue.Get()
		if quit {
			return nil
		}
		var err error
		if name, ok := strings.CutPrefix(k, "pvc/"); ok {
			err = pr.syncClaim(ctx, name)
		} else if name, ok := strings.CutPrefix(k, "pv/"); ok {
			err = pr.syncVolume(ctx, name)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "tabernetes: provisioner:", k+":", err)
			pr.queue.AddRateLimited(k)
		} else {
			pr.queue.Forget(k)
		}
		pr.queue.Done(k)
	}
}

// syncClaim creates the volume of a claim that waits for this provisioner.
func (pr *provisioner) syncClaim(ctx context.Context, key string) error {
	o, ok, _ := pr.claims.GetByKey(key)
	if !ok {
		return nil
	}
	pvc := o.(*corev1.PersistentVolumeClaim)
	if pvc.Spec.VolumeName != "" || pvc.Annotations[annProvisioner] != provisionerName || pvc.DeletionTimestamp != nil {
		return nil
	}
	class := ptrOr(pvc.Spec.StorageClassName, "")
	co, ok, _ := pr.classes.GetByKey(class)
	if !ok {
		return nil
	}
	sc := co.(*storagev1.StorageClass)
	if sc.VolumeBindingMode != nil && *sc.VolumeBindingMode == storagev1.VolumeBindingWaitForFirstConsumer && pvc.Annotations[annSelectedNode] == "" {
		return nil // the scheduler sets the node when a pod uses the claim
	}
	if pvc.Spec.VolumeMode != nil && *pvc.Spec.VolumeMode == corev1.PersistentVolumeBlock {
		return pr.event(ctx, pvc, "ProvisioningFailed", fmt.Sprintf("failed to provision volume with StorageClass %q: the tab node has file system volumes only", class))
	}
	pv := newVolume(pvc, sc)
	if _, err := pr.cs.CoreV1().PersistentVolumes().Create(ctx, pv, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return pr.event(ctx, pvc, "ProvisioningSucceeded", fmt.Sprintf("Successfully provisioned volume %s", pv.Name))
}

// newVolume is the PV of a claim, named like the ones of external
// provisioners.
func newVolume(pvc *corev1.PersistentVolumeClaim, sc *storagev1.StorageClass) *corev1.PersistentVolume {
	name := "pvc-" + string(pvc.UID)
	reclaim := corev1.PersistentVolumeReclaimDelete
	if sc.ReclaimPolicy != nil {
		reclaim = *sc.ReclaimPolicy
	}
	return &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: name, Annotations: map[string]string{annProvisionedBy: provisionerName}},
		Spec: corev1.PersistentVolumeSpec{
			Capacity:                      corev1.ResourceList{corev1.ResourceStorage: pvc.Spec.Resources.Requests[corev1.ResourceStorage]},
			AccessModes:                   pvc.Spec.AccessModes,
			PersistentVolumeReclaimPolicy: reclaim,
			StorageClassName:              sc.Name,
			MountOptions:                  sc.MountOptions,
			VolumeMode:                    pvc.Spec.VolumeMode,
			ClaimRef: &corev1.ObjectReference{
				Kind: "PersistentVolumeClaim", APIVersion: "v1",
				Namespace: pvc.Namespace, Name: pvc.Name, UID: pvc.UID, ResourceVersion: pvc.ResourceVersion,
			},
			PersistentVolumeSource: corev1.PersistentVolumeSource{Local: &corev1.LocalVolumeSource{Path: volumesPath + name}},
			// every tab node has the OPFS of the browser, no node from join
			NodeAffinity: &corev1.VolumeNodeAffinity{Required: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{
				MatchExpressions: []corev1.NodeSelectorRequirement{{Key: corev1.LabelOSStable, Operator: corev1.NodeSelectorOpIn, Values: []string{"browser"}}},
			}}}},
		},
	}
}

// syncVolume deletes a released volume of this provisioner with the
// reclaim policy Delete, and its data.
func (pr *provisioner) syncVolume(ctx context.Context, name string) error {
	o, ok, _ := pr.pvs.GetByKey(name)
	if !ok {
		return nil
	}
	pv := o.(*corev1.PersistentVolume)
	if pv.Annotations[annProvisionedBy] != provisionerName || pv.Spec.PersistentVolumeReclaimPolicy != corev1.PersistentVolumeReclaimDelete ||
		pv.Status.Phase != corev1.VolumeReleased || pv.DeletionTimestamp != nil {
		return nil
	}
	if volumeInUse(pv.Name) {
		return fmt.Errorf("volume %s is still in use by a pod", pv.Name)
	}
	if err := removeVolumeData(ctx, pv.Name); err != nil {
		return fmt.Errorf("deleting the data of %s: %w", pv.Name, err)
	}
	err := pr.cs.CoreV1().PersistentVolumes().Delete(ctx, pv.Name, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

func (pr *provisioner) event(ctx context.Context, pvc *corev1.PersistentVolumeClaim, reason, msg string) error {
	typ := corev1.EventTypeNormal
	if reason == "ProvisioningFailed" {
		typ = corev1.EventTypeWarning
	}
	now := metav1.NewTime(time.Now())
	_, err := pr.cs.CoreV1().Events(pvc.Namespace).Create(ctx, &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{GenerateName: pvc.Name + ".", Namespace: pvc.Namespace},
		InvolvedObject: corev1.ObjectReference{Kind: "PersistentVolumeClaim", APIVersion: "v1", Namespace: pvc.Namespace, Name: pvc.Name, UID: pvc.UID, ResourceVersion: pvc.ResourceVersion},
		Reason:         reason,
		Message:        msg,
		Type:           typ,
		Source:         corev1.EventSource{Component: provisionerName + "_tabernetes"},
		FirstTimestamp: now, LastTimestamp: now, Count: 1,
	}, metav1.CreateOptions{})
	return err
}

// removeVolumeData deletes tabernetes/volumes/<name> from the OPFS.
func removeVolumeData(ctx context.Context, name string) error {
	f := js.Global().Get("tabernetesRemoveVolume")
	if f.Type() != js.TypeFunction {
		return nil // no OPFS here: nothing was kept
	}
	done := make(chan error, 1)
	var ok, fail js.Func
	end := func(err error) {
		done <- err
		ok.Release()
		fail.Release()
	}
	ok = js.FuncOf(func(js.Value, []js.Value) any { end(nil); return nil })
	fail = js.FuncOf(func(_ js.Value, a []js.Value) any {
		end(fmt.Errorf("%s", a[0].Call("toString").String()))
		return nil
	})
	f.Invoke(name).Call("then", ok, fail)
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// The volumes that pods of the tab nodes use: a pod worker holds the data of
// a volume while it runs, so two pods at once would overwrite each other.
type volumeUser struct {
	tp  *tabPod
	gen int // the run of the sandbox (v86Pod.gen)
}

var (
	volumeUsersMu sync.Mutex
	volumeUsers   = map[string]volumeUser{} // PV name -> pod
)

// volumeInUse tells whether a pod of a tab node uses the volume.
func volumeInUse(pv string) bool {
	volumeUsersMu.Lock()
	defer volumeUsersMu.Unlock()
	return volumeUsers[pv].tp != nil
}

// releaseVolumes ends the use of the volumes of a sandbox of tp, once its
// worker wrote them back.
func (tp *tabPod) releaseVolumes(gen int) {
	volumeUsersMu.Lock()
	defer volumeUsersMu.Unlock()
	for pv, u := range volumeUsers {
		if u.tp == tp && u.gen == gen {
			delete(volumeUsers, pv)
		}
	}
}

// claimVolume returns the PV of the claim name of the pod, once it is bound
// to a volume of this provisioner, and takes it for the pod.
func (tp *tabPod) claimVolume(ctx context.Context, name string) (string, error) {
	_, cs, err := podNetClient()
	if err != nil {
		return "", err
	}
	pvc, err := cs.CoreV1().PersistentVolumeClaims(tp.namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	if pvc.Spec.VolumeName == "" || pvc.Status.Phase != corev1.ClaimBound {
		return "", fmt.Errorf("PVC %s/%s is not bound", tp.namespace, name)
	}
	pv, err := cs.CoreV1().PersistentVolumes().Get(ctx, pvc.Spec.VolumeName, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	if pv.Annotations[annProvisionedBy] != provisionerName {
		return "", fmt.Errorf("the tab node has only volumes of the StorageClass of %s, not %s", provisionerName, pv.Name)
	}
	tp.mu.Lock()
	gen := tp.vmGen
	tp.mu.Unlock()
	volumeUsersMu.Lock()
	defer volumeUsersMu.Unlock()
	switch u := volumeUsers[pv.Name]; {
	case u.tp != nil && u.tp != tp:
		// the words of the attach/detach controller
		return "", fmt.Errorf("Multi-Attach error for volume %q Volume is already used by pod(s) %s", pv.Name, u.tp.name)
	case u.tp == tp && u.gen != gen:
		return "", fmt.Errorf("volume %q is still being written back by the last sandbox of the pod", pv.Name)
	}
	volumeUsers[pv.Name] = volumeUser{tp, gen}
	return pv.Name, nil
}

// sweep deletes the data of volumes that have no PV anymore: a cluster that
// did not boot from its snapshot, or a delete that broke off.
// Only the control plane in the SharedWorker sweeps: it is the one cluster
// of the origin. Without it, every tab runs its own cluster on the same
// storage, and the volumes of the other tabs have no PV here.
func (pr *provisioner) sweep(ctx context.Context) {
	f := js.Global().Get("tabernetesVolumeNames")
	sw := js.Global().Get("SharedWorkerGlobalScope")
	if f.Type() != js.TypeFunction || sw.Type() != js.TypeFunction || !js.Global().InstanceOf(sw) {
		return
	}
	names := make(chan []string, 1)
	var then js.Func
	then = js.FuncOf(func(_ js.Value, a []js.Value) any {
		var l []string
		for i := 0; i < a[0].Length(); i++ {
			l = append(l, a[0].Index(i).String())
		}
		names <- l
		then.Release()
		return nil
	})
	f.Invoke().Call("then", then)
	var l []string
	select {
	case l = <-names:
	case <-ctx.Done():
		return
	}
	for _, name := range l {
		if _, ok, _ := pr.pvs.GetByKey(name); ok || volumeInUse(name) {
			continue
		}
		if _, err := pr.cs.CoreV1().PersistentVolumes().Get(ctx, name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
			continue
		}
		if err := removeVolumeData(ctx, name); err != nil {
			fmt.Fprintln(os.Stderr, "tabernetes: provisioner: deleting the data of", name+":", err)
		}
	}
}
