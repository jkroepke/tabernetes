# Spring Boot on a tab node

A small Spring Boot 4 web app (Tomcat, one `GET /`) as a `linux/386` image,
so that it runs in v86 on the tab node.

```shell
docker buildx build --platform linux/386 -t jkroepke/tabernetes-spring-boot examples/spring-boot
```

In the terminal of the page:

```shell
kubectl apply -f deployment.yaml
kubectl logs -f deploy/spring-boot
kubectl port-forward deploy/spring-boot 8080:8080 &
wget -qO- localhost:8080
```

## What it takes

- **A Java runtime for 32-bit x86.** No vendor ships a `linux/386` image of
  a JRE (Temurin, Corretto, Zulu, Liberica, SapMachine, Semeru). OpenJDK
  dropped the 32-bit x86 port in Java 25, so Java 21 is the newest one.
  Ubuntu 24.04 still builds it for i386. The Dockerfile links the i386
  modules with `jlink` into a runtime of 67 MB on top of
  `debian:trixie-slim`. The image is about 100 MB compressed.
- **Time.** Code in the VM runs about 40 times slower than on the host.
  The app answers HTTP about 50 s after the start (natively 0.7 s), when the
  image is already in the browser.
  Measured in Node.js with the v86 build, kernel and init of Tabernetes:

  | Setup | HTTP 200 after |
  | --- | --- |
  | JVM defaults, AppCDS, with actuator | 133 s |
  | C1 only (`-XX:TieredStopAtLevel=1`), serial GC | 70 s |
  | and Spring AOT | 65 s |
  | and without actuator (this example) | 51 s |

  `-Xint` (80 s) and lazy initialization (no gain) did not help. Most of the
  time is the JVM itself: `java -version` alone takes 3.4 s.
- **Memory.** About 100 MiB in the VM. It runs with the 256 MiB that a pod
  gets without a memory limit.
- **No probes.** The kubelet of a tab has no probes, so the pod is Ready as
  soon as the JVM starts, about 50 s before Tomcat listens. A port-forward
  in that time gets "connection refused".
