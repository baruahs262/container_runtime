# crt

A container runtime written from scratch in Go, using only the standard
library. It isolates processes with Linux namespaces, confines them with
cgroups, and runs them on an overlay filesystem stacked from
content-addressed image layers. It can build, pull, ship and
garbage-collect those images, and it monitors every container from
background goroutines.

```
$ sudo crt pull alpine:3.20
$ sudo crt run --rm --cpu 0.5 --memory 64m --stats alpine:3.20 sh -c 'echo hi from $(hostname)'
hi from 8bec83b73065
--- 8bec83b73065: exit 0 in 8ms (startup 19.355ms)
    cpu    avg 0.0%  peak 0.0%  limit 50%  throttled 0 periods  total 4ms
    memory peak 964.0KiB  limit 64.0MiB  oom kills 0
    samples 0
```

## Features

- **Isolation**: new mount, UTS, IPC, PID and cgroup namespaces, plus an
  optional network namespace with loopback only. The root switch uses
  `pivot_root` (the default) or plain `chroot`. Sensitive `/proc` and
  `/sys` paths are masked and dangerous capabilities are dropped from
  the bounding set.
- **Resource limits**: CPU quota in cores, memory with swap disabled, and
  a process count cap. cgroup v1 and v2 are both supported behind one
  interface.
- **Monitoring**: a sampling goroutine per container turns cgroup
  counters into CPU %, memory, throttling and OOM events. OOM kills
  arrive through an eventfd (v1) or inotify (v2) notifier. A fan-in hub
  drives `crt stats` across all running containers.
- **Layered images**: layers are directories named by a content hash,
  stored with overlay-native whiteouts so they mount directly as overlay
  lower dirs. Identical content is stored once.
- **Build**: a Dockerfile-style `Buildfile` supports FROM, RUN, COPY/ADD,
  ENV, WORKDIR, CMD, ENTRYPOINT and LABEL. Each RUN step executes in a
  real container, and its overlay upper dir becomes the new layer. A
  chained step key drives the build cache.
- **Pull**: an OCI distribution client with anonymous bearer auth,
  multi-arch index resolution, and concurrent, digest-verified layer
  downloads.
- **Ship and maintain**: `image save`/`load` bundles are self-verifying.
  Also available: rootfs `import`/`export`, `squash`, `commit` from a
  container, `tag`, `history`, `inspect`, and `gc`.
- **Lifecycle**: foreground or detached (`-d`) containers, with `ps`,
  `logs`, `stop`, `kill`, `rm`, `prune` and `inspect`. There is an
  optional reaping init (`--init`) that forwards signals.
- **Benchmarks**: `crt bench` reports throughput and latency percentiles
  against a host fork/exec baseline. It also measures CPU and memory
  limit accuracy. Results are in [BENCHMARKS.md](BENCHMARKS.md).

## Requirements

- Linux with overlayfs and cgroups (v1 or v2), running as root.
- Go 1.24 or newer to build. The binaries are static (`CGO_ENABLED=0`).

## Build and test

```
make build          # bin/crt and bin/crt-stress
make unit           # unit tests; root-only tests skip themselves
make test           # everything, as root: namespaces, OOM, quotas, builds
make bench          # throughput + enforcement report
```

State lives in `/var/lib/crt`. Set `CRT_ROOT` or pass `--root` to use
another directory.

## Usage

```
crt run [OPTIONS] IMAGE [COMMAND...]
    --cpu 0.5            CPU cores
    --memory 128m        memory limit, swap disabled
    --pids 64            max processes
    --network none       private netns with loopback (default: host)
    --isolation chroot   chroot instead of pivot_root
    -v /src:/dst[:ro]    bind mount
    -e K=V  -w DIR       environment, working directory
    --hostname NAME      default: container id
    --read-only          read-only root filesystem
    --init               reaping, signal-forwarding PID 1
    --privileged         keep all capabilities
    -d  --rm  --stats    detach, auto-remove, print a usage summary

crt ps [-a] | logs [-f] ID | stats [ID...] | stop [-t S] ID | kill [-s SIG] ID
crt rm [-f] ID | prune | inspect ID | commit ID NAME:TAG

crt build -t NAME:TAG [-f FILE] [--no-cache] [-v HOST:CTR] CONTEXT
crt pull [--arch ARCH] NAME:TAG
crt images | image rm|tag|history|inspect|squash ...
crt image save -o FILE IMAGE | image load -i FILE
crt image import ROOTFS.tar[.gz] NAME:TAG | image export -o FILE IMAGE
crt gc
crt bench [-n 200] [-parallel 1,4,8,16] [-scenarios bare,limits,monitored,netns]
```

Container ids accept any unique prefix.

### Example: build and deploy

```
$ cat examples/Buildfile.alpine
FROM alpine:3.20
ENV APP_HOME=/srv/app
RUN apk add --no-cache curl jq
WORKDIR $APP_HOME
COPY webapp/ ./
RUN chmod +x serve.sh && rm -f /etc/motd
CMD ["./serve.sh"]

$ sudo crt build -t webapp:1 -f examples/Buildfile.alpine examples
$ sudo crt run --rm webapp:1
$ sudo crt image save -o webapp.bundle webapp:1     # copy to another host
$ sudo crt image load -i webapp.bundle              # layers are re-verified
```

`crt build -v HOST:CONTAINER[:ro]` mounts host paths into every RUN step
without committing them. That covers caches, secrets and extra CA
certificates behind a TLS-intercepting proxy.

## How it works

```
crt run                          (parent, host namespaces)
 ├─ create cgroup crt/<id>, write limits
 ├─ clone /proc/self/exe as "crt-init" with CLONE_NEWNS|NEWUTS|NEWIPC|NEWPID[|NEWNET]
 │    fd 3: spec JSON   fd 4: release pipe   fd 5: error pipe (close-on-exec)
 ├─ write child pid to cgroup.procs
 ├─ start OOM watcher + sampling goroutines
 ├─ close release pipe ───────────────► crt-init (PID 1 in the new PID namespace)
 │                                        unshare(CLONE_NEWCGROUP)
 │                                        make / private (MS_REC|MS_PRIVATE)
 │                                        mount overlay lower=layers upper work
 │                                        mount proc, sysfs (ro), tmpfs /dev,
 │                                          devpts, shm, mqueue, device binds
 │                                        sethostname, lo up (netns)
 │                                        pivot_root(".", ".") + detach old root
 │                                        mask /proc paths, drop capabilities
 │                                        execve(workload)  ── error pipe closes
 ├─ wait, map status to exit code (128+signal, OOM from the notifier)
 └─ kill leftovers, remove cgroup, record state
```

The runtime re-executes its own binary for the init role, so there is no
separate helper. The switch happens in an `init()` function of the
`container` package, and any Go binary that links the package can spawn
containers, including its tests. Detached containers are supervised by
the same binary in a third role, `crt-shim`. It runs in its own session
and writes output to `containers/<id>/container.log`.

The overlay is mounted inside the container's private mount namespace.
It disappears with the last process, so the host never has to unmount
anything.

### Storage layout

```
/var/lib/crt/
  layers/<sha256>/fs           layer contents: overlay whiteouts (char 0:0),
  layers/<sha256>/layer.json     opaque dirs (trusted.overlay.opaque=y)
  images/<name:tag>.json       layers bottom-first, config, history
  cache/<step key>             build cache: step key -> layer
  blobs/<digest>               registry blob -> layer, so re-pulls skip downloads
  containers/<id>/             state.json, spec.json, upper/, work/, rootfs/, log
```

A layer id is a SHA-256 over paths, types, modes, owners, file contents,
link targets, device numbers, opaque markers and file capabilities. It
deliberately excludes timestamps, so rebuilding identical content reuses
the same layer. Pulled blobs are verified against their registry digest.
Bundle layers are verified by re-hashing on load, so a tampered bundle is
rejected.

When a RUN step's upper dir becomes a layer, three things are stripped.
Empty mount-point stubs the runtime created (`/proc`, `/sys`, `/dev`,
`/etc/resolv.conf` and bind targets) are removed. Directories that were
only copied up are removed. Overlay bookkeeping xattrs are removed. A
no-op RUN therefore adds no layer.

## Limitations

- **No user namespaces.** Mounting an overlay with `trusted.*` opaque
  markers needs real root, and a multi-threaded Go process cannot
  `unshare(CLONE_NEWUSER)` late. Container root is host root, reduced
  only by the capability bounding set.
- **No seccomp, AppArmor or SELinux profiles.**
- **Networking is host or none.** There is no bridge or veth setup and
  no port mapping.
- **`--isolation chroot` is weaker than `pivot_root`.** The host root
  stays mounted in the namespace, and a process with CAP_SYS_CHROOT can
  escape.
- **Registry support**: anonymous pulls only; no push. zstd-compressed
  layers are rejected. Docker Hub rate-limits anonymous pulls, and
  `mirror.gcr.io/library/<image>` serves the same images.
- **Buildfile**: single stage only. No USER, ARG, EXPOSE, VOLUME,
  HEALTHCHECK, or ADD from URLs or archives.
- **`--init` threads count toward `--pids`.** The Go init starts a few
  runtime threads, and each one counts against the limit.
