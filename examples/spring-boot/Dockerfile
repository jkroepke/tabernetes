# A Spring Boot app for the tab node of Tabernetes: a linux/386 image with a
# small Java 21 runtime.
#
#   docker buildx build --platform linux/386 -t <name> examples/spring-boot
#
# No vendor publishes a Java runtime image for linux/386 any more. Ubuntu
# 24.04 still builds OpenJDK 21 for i386, so the runtime comes from there:
# jlink of the build platform links the i386 modules into a runtime of 67 MB
# (both come from the same OpenJDK build, as jlink requires). The build
# platform must be linux/amd64, the i386 packages are not on the ports mirror
# of the other architectures. Java 21 is the last version with a 32-bit x86
# port.
ARG UBUNTU_IMAGE=mirror.gcr.io/library/ubuntu:24.04
ARG MAVEN_IMAGE=mirror.gcr.io/library/maven:3-eclipse-temurin-21
ARG RUNTIME_IMAGE=mirror.gcr.io/library/debian:trixie-slim

FROM --platform=$BUILDPLATFORM ${MAVEN_IMAGE} AS app
WORKDIR /src
COPY pom.xml .
RUN --mount=type=cache,target=/root/.m2 mvn -B -q dependency:go-offline
COPY src src
RUN --mount=type=cache,target=/root/.m2 mvn -B -q package -DskipTests \
  && java -Djarmode=tools -jar target/demo.jar extract --destination /app

FROM --platform=$BUILDPLATFORM ${UBUNTU_IMAGE} AS jre
ARG DEBIAN_FRONTEND=noninteractive
WORKDIR /tmp/deb
RUN dpkg --add-architecture i386 \
  && apt-get update \
  && apt-get install -y --no-install-recommends binutils openjdk-21-jdk-headless \
  && apt-get download openjdk-21-jdk-headless:i386 openjdk-21-jre-headless:i386 \
  && for d in *.deb; do dpkg -x "$d" /i386; done \
  && rm -rf /var/lib/apt/lists/*
# The modules that jdeps finds for Spring Boot, plus jdk.management (the GC
# metrics of Micrometer) and jdk.crypto.ec and jdk.zipfs.
RUN jlink --module-path /i386/usr/lib/jvm/java-21-openjdk-i386/jmods \
      --add-modules java.base,java.compiler,java.desktop,java.instrument,java.management,java.naming,java.net.http,java.prefs,java.scripting,java.security.jgss,java.sql,jdk.crypto.ec,jdk.jfr,jdk.management,jdk.unsupported,jdk.zipfs \
      --strip-debug --no-man-pages --no-header-files --compress=zip-6 --output /jre

FROM ${RUNTIME_IMAGE}
ENV JAVA_HOME=/opt/java PATH=/opt/java/bin:$PATH
COPY --from=jre /jre /opt/java
COPY --from=app /app /app
WORKDIR /app
# The VM of a pod is about 40 times slower than the host: the JVM starts with
# the C1 compiler only (C2 costs more than it saves in the first minute) and
# the serial GC (one CPU). Class data sharing (CDS) maps the classes of the
# JDK and of the app instead of loading them from the jars: the base archive
# for the JDK, a training run that stops after the context refresh for the
# app.
RUN java -Xshare:dump >/dev/null \
  && java -XX:TieredStopAtLevel=1 -XX:+UseSerialGC -Dspring.aot.enabled=true \
      -XX:ArchiveClassesAtExit=/app/demo.jsa -Dspring.context.exit=onRefresh -jar /app/demo.jar
EXPOSE 8080
ENTRYPOINT ["java", "-XX:TieredStopAtLevel=1", "-XX:+UseSerialGC", "-XX:SharedArchiveFile=/app/demo.jsa", "-Dspring.aot.enabled=true", "-jar", "/app/demo.jar"]
