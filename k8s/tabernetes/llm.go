package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"mvdan.cc/sh/v3/interp"
)

// llm makes the tab a node (tabnode.go), runs a language model on its GPU as
// the pod "llm" (webllm.go) and chats with it through kubectl exec.

const llmManifest = `apiVersion: v1
kind: Pod
metadata:
  name: llm
  namespace: default
  labels: {app: llm}
spec:
  # The tab node, nowhere else.
  nodeSelector:
    kubernetes.io/os: browser
  tolerations:
  - {key: tabernetes.dev/tab, operator: Exists}
  containers:
  - name: llm
    image: webllm:%s
    ports:
    - {name: http, containerPort: 8000}
    resources:
      limits:
        tabernetes.dev/webgpu: 1
`

func llmCmd(ctx context.Context, hc interp.HandlerContext, args []string) error {
	if len(args) > 1 || (len(args) == 1 && strings.HasPrefix(args[0], "-")) {
		fmt.Fprintln(hc.Stderr, "usage: llm [model]")
		fmt.Fprintln(hc.Stderr, "Runs a WebLLM model (https://webllm.mlc.ai) on the GPU of this tab, default "+defaultModel+".")
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
	model := ""
	if len(args) == 1 {
		model = args[0]
	}
	if model == "" {
		model = defaultModel
		if !n.gpu.f16 {
			// The GPU has no 16 bit floats in shaders.
			model = strings.Replace(model, "q4f16_1", "q4f32_1", 1)
		}
	}
	image := "webllm:" + model

	pods := n.cs.CoreV1().Pods(corev1.NamespaceDefault)
	pod, err := pods.Get(ctx, "llm", metav1.GetOptions{})
	ready := false
	switch {
	case err == nil && pod.DeletionTimestamp == nil:
		if len(args) == 1 && pod.Spec.Containers[0].Image != image {
			return fail("the pod llm runs %s. Run \"kubectl delete pod llm\" first.", pod.Spec.Containers[0].Image)
		}
		image = pod.Spec.Containers[0].Image
		ready = podReady(pod)
	default:
		manifest := fmt.Sprintf(llmManifest, model)
		if code := runKubectl(ctx, []string{"apply", "-f", "-"}, strings.NewReader(manifest), hc.Stdout, hc.Stderr); code != 0 {
			return exitCode(code)
		}
	}

	if !ready {
		fmt.Fprintf(hc.Stdout, "Loading %s onto %s. The first time, the weights come from Hugging Face (about 1 GB), then from the browser cache.\n", image, n.gpu)
		logCtx, stopLogs := context.WithCancel(ctx)
		go func() {
			// the pod may not be bound yet
			for i := 0; i < 100 && logCtx.Err() == nil; i++ {
				if code := runKubectl(logCtx, []string{"logs", "-f", "llm"}, nil, hc.Stdout, io.Discard); code == 0 || logCtx.Err() != nil {
					return
				}
				time.Sleep(200 * time.Millisecond)
			}
		}()
		err = waitLLM(ctx, n)
		stopLogs()
		if err != nil {
			return fail("%v", err)
		}
	}

	fmt.Fprintln(hc.Stdout, "Each line goes to the pod: kubectl exec -i llm -- chat. Ctrl-C stops, Ctrl-D ends.")
	fmt.Fprintln(hc.Stdout, "The OpenAI API: kubectl port-forward pod/llm 8000, then proxy 8000 or wget.")
	err = llmChat(ctx)
	fmt.Fprintln(hc.Stdout, "The pod keeps running: llm chats again, kubectl delete pod llm frees the GPU.")
	return err
}

// waitLLM waits until the pod is ready, and gives up when it fails.
func waitLLM(ctx context.Context, n *tabNode) error {
	pods := n.cs.CoreV1().Pods(corev1.NamespaceDefault)
	for {
		pod, err := pods.Get(ctx, "llm", metav1.GetOptions{})
		if err != nil {
			return err
		}
		if podReady(pod) {
			return nil
		}
		if pod.Status.Phase == corev1.PodFailed {
			return fmt.Errorf("the pod failed: %s", pod.Status.Message)
		}
		for _, cs := range pod.Status.ContainerStatuses {
			if w := cs.State.Waiting; w != nil && (w.Reason == "CrashLoopBackOff" || w.Reason == "ErrImagePull") {
				msg := w.Message
				if t := cs.LastTerminationState.Terminated; t != nil {
					msg = t.Message
				}
				return fmt.Errorf("%s: %s (kubectl describe pod llm)", w.Reason, msg)
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// llmChat is a small line editor in front of "kubectl exec -i llm -- chat".
// The terminal has no tty for kubectl exec, so the lines are edited here and
// the pod prints the prompt. Ctrl-C goes to the pod as \x03, like a tty.
func llmChat(ctx context.Context) error {
	execCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	stdin, toPod := io.Pipe()
	done := make(chan int, 1)
	go func() { done <- runKubectl(execCtx, []string{"exec", "-i", "llm", "--", "chat"}, stdin, tty, tty) }()

	leave := tty.enterLine()
	defer leave()
	lines := make(chan string, 16)
	go func() {
		for l := range lines {
			if _, err := io.WriteString(toPod, l); err != nil {
				return
			}
		}
		toPod.Close()
	}()
	var line []rune
	closed := false
	for {
		select {
		case code := <-done:
			if !closed {
				close(lines)
			}
			stdin.Close()
			return exitCode(code)
		case <-ctx.Done():
			// kubectl exec does not stop with the context: end its stdin.
			if !closed {
				lines <- "\x03\n"
				close(lines)
			}
			stdin.Close()
			cancel()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
			}
			return ctx.Err()
		case s := <-tty.keys:
			for _, k := range parseKeys(s) {
				switch {
				case closed:
				case k.name == "ctrl-c":
					// as a tty would: the pod reads it from stdin
					io.WriteString(tty, "^C\n")
					closed = true
					lines <- "\x03\n"
					close(lines)
				case k.name == "ctrl-d" && len(line) == 0:
					io.WriteString(tty, "\n")
					closed = true
					close(lines)
				case k.name == "enter":
					io.WriteString(tty, "\n")
					lines <- string(line) + "\n"
					line = nil
				case k.name == "backspace":
					if len(line) > 0 {
						line = line[:len(line)-1]
						io.WriteString(tty, "\b \b")
					}
				case k.name == "" && k.r != 0:
					line = append(line, k.r)
					io.WriteString(tty, string(k.r))
				}
			}
		}
	}
}

func podReady(pod *corev1.Pod) bool {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}
