package main

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"mvdan.cc/sh/v3/interp"
)

// llm runs a language model on the GPU of a tab as the Deployment "llm":
// it applies the manifest, like kubectl create deployment, and that is all.
// The tab node (tabnode.go) pulls the image huggingface.co/mlc-ai/<model>
// (webllm.go), and
// kubectl exec -it deploy/llm -- chat talks to it.

const llmManifest = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: llm
  namespace: default
  labels: {app: llm}
spec:
  replicas: 1
  # one GPU: the old pod goes first
  strategy: {type: Recreate}
  selector:
    matchLabels: {app: llm}
  template:
    metadata:
      labels: {app: llm}
    spec:
      # A tab with WebGPU: the resource below picks one. The scheduler
      # prefers this tab, and moves the pod to another one when it closes.
      nodeSelector:
        kubernetes.io/os: browser
      affinity:
        nodeAffinity:
          preferredDuringSchedulingIgnoredDuringExecution:
          - weight: 100
            preference:
              matchExpressions:
              - {key: kubernetes.io/hostname, operator: In, values: [%s]}
      tolerations:
      - {key: tabernetes.dev/tab, operator: Exists}
      containers:
      - name: llm
        image: huggingface.co/mlc-ai/%s
        ports:
        - {name: http, containerPort: 8000}
        resources:
          limits:
            tabernetes.dev/webgpu: 1
`

func llmCmd(ctx context.Context, hc interp.HandlerContext, args []string) error {
	if len(args) > 1 || (len(args) == 1 && strings.HasPrefix(args[0], "-")) {
		fmt.Fprintln(hc.Stderr, "usage: llm [model]")
		fmt.Fprintln(hc.Stderr, "Deploys a WebLLM model (https://webllm.mlc.ai) to the GPU of a tab, default "+defaultModel+".")
		return interp.ExitStatus(2)
	}
	fail := func(format string, a ...any) error {
		fmt.Fprintf(hc.Stderr, "llm: "+format+"\n", a...)
		return interp.ExitStatus(1)
	}
	n, err := startTabNode(ctx)
	if err != nil {
		return fail("%v", err)
	}
	if n.gpu == nil {
		return fail("this browser has no WebGPU adapter (Chrome or Edge on a machine with a GPU has one)")
	}
	model := ""
	if len(args) == 1 {
		model = args[0]
	} else if d, err := n.cs.AppsV1().Deployments(corev1.NamespaceDefault).Get(ctx, "llm", metav1.GetOptions{}); err == nil {
		// keep the model of the deployment
		if c := d.Spec.Template.Spec.Containers; len(c) == 1 {
			model, _ = webllmModel(c[0].Image)
		}
	}
	if model == "" {
		model = defaultModel
		if !n.gpu.f16 {
			// The GPU has no 16 bit floats in shaders.
			model = strings.Replace(model, "q4f16_1", "q4f32_1", 1)
		}
	}
	manifest := fmt.Sprintf(llmManifest, tabNodeName, model)
	if code := runKubectl(ctx, []string{"apply", "-f", "-"}, strings.NewReader(manifest), hc.Stdout, hc.Stderr); code != 0 {
		return exitCode(code)
	}
	fmt.Fprintln(hc.Stdout, "A tab with WebGPU pulls the model like an image, about 1 GB the first time.")
	fmt.Fprintln(hc.Stdout, "Watch it with kubectl get pods -w or kubectl describe pod -l app=llm, then:")
	fmt.Fprintln(hc.Stdout, "  kubectl exec -it deploy/llm -- chat")
	fmt.Fprintln(hc.Stdout, "  kubectl port-forward deploy/llm 8000 &, then proxy 8000 for a chat page or wget for the OpenAI API")
	return nil
}
