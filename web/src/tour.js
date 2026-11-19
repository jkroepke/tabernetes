// tour walks a first-time visitor through what the cluster can do
// (terminal.js). Each step explains a little and puts its command on the
// prompt: Enter runs it, the visitor may edit it first, Ctrl-C ends the tour.
// The commands are plain kubectl, so the visitor can type them again later.
// The last step offers to join a real node (join.js).

const B = (s) => `\x1b[1m${s}\x1b[0m`;
const C = (s) => `\x1b[36m${s}\x1b[0m`;

// The image of the tour: small, has a linux/386 variant and busybox wget.
const IMAGE = "nginx:alpine";

export const steps = [
  {
    title: "A real Kubernetes in this tab",
    text: [
      "kube-apiserver, kube-controller-manager, kube-scheduler and kubectl are the",
      "upstream code, compiled to WebAssembly. They run in this browser, not on a server.",
    ],
    cmd: "kubectl version",
  },
  {
    title: "Every tab is a node",
    text: [
      "This tab is a node of the cluster. Open the page in a second tab and it joins",
      "as another node; the cluster lives as long as one tab is open.",
    ],
    cmd: "kubectl get nodes -o wide",
  },
  {
    title: "Run a container image from Docker Hub",
    text: [
      `The tab pulls the linux/386 variant of ${IMAGE} and runs every pod in its own`,
      "Linux VM (v86, an x86 emulator in WebAssembly). The image stays in the browser.",
    ],
    cmd: `kubectl create deployment web --image=${IMAGE} --replicas=2`,
  },
  {
    title: "Wait for the rollout",
    text: ["The Deployment controller makes a ReplicaSet, the scheduler places its pods."],
    cmd: "kubectl rollout status deployment/web",
  },
  {
    title: "Pods with their own IP",
    text: ["Each pod has an IP of the pod CIDR of its node."],
    cmd: "kubectl get pods -o wide",
  },
  {
    title: "A Service in front of the pods",
    text: ["A ClusterIP Service balances over the ready pods of the Deployment."],
    cmd: "kubectl expose deployment web --port=80",
  },
  {
    title: "Cluster DNS and kubectl exec",
    text: [
      "kubectl exec runs wget in one of the pods. It finds the Service by its DNS name,",
      "and the request goes to one of the nginx pods.",
    ],
    cmd: "kubectl exec deploy/web -- wget -qO- http://web | grep title",
  },
  {
    title: "Logs",
    text: ["The console of the VM is the log of the container."],
    cmd: "kubectl logs deploy/web --tail=5",
  },
  {
    title: "Self-healing",
    text: ["Delete the pods: the ReplicaSet controller makes new ones at once."],
    cmd: "kubectl delete pods -l app=web --wait=false",
  },
  {
    title: "New pods",
    text: ["The new pods start from the image that the browser keeps: no second download."],
    cmd: "kubectl get pods",
  },
  {
    title: "Clean up",
    text: ["Tab nodes run up to 8 pods each. Make room for your own."],
    cmd: "kubectl delete deployment,service web",
  },
];

// more is said after the last step, before the question about join.
const more = [
  `${B("More to try")}`,
  `  ${C("kubectl port-forward deploy/<name> 8080:80 &")} then ${C("proxy 8080")}  opens a port of a pod in a new tab`,
  `  ${C("llm")}   a language model on the GPU of this tab (WebGPU), as a Deployment`,
  `  ${C("doom")}  KubeDoom: shoot the pods of the default namespace`,
  `  ${C("help")}  all commands`,
];

// intro returns the lines that open the step i.
export function intro(i) {
  const s = steps[i];
  const lines = [""];
  if (i === 0) {
    lines.push(DIMMED("Tour: Enter runs the command on the prompt (edit it first if you like), Ctrl-C ends the tour."), "");
  }
  lines.push(`${B(`[${i + 1}/${steps.length}] ${s.title}`)}`, ...s.text.map((t) => DIMMED(t)));
  return lines;
}

const DIMMED = (s) => `\x1b[2m${s}\x1b[0m`;

// outro returns the lines after the last step.
export function outro() {
  return [
    "",
    ...more,
    "",
    `${B("Join a real node")}`,
    DIMMED("join prints a docker run command. The container joins this cluster over WebRTC as a real"),
    DIMMED("node with the kubelet and containerd: amd64 and arm64 images, every volume type, NodePorts."),
  ];
}

export const joinQuestion = "Connect a node with Docker now? [y/N] ";
