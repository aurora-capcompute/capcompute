# Architecture

This document describes the OS model of the Aurora core. It has two parts:

- **capcompute**, the processor. It runs Wasm programs deterministically behind
  one syscall gate.
- **aurora-capcompute**, the runtime on top. Its `monitor/`, `replay/` and
  `journaled/` packages are the reference monitor, replay and the journal.

Together they are a **library operating system**: OS abstractions as Go
libraries that a host application links in. The model has four properties:

- **Capability-based.** A guest has no ambient authority. It can only call
  capabilities that its grant names (lineage: seL4, KeyKOS).
- **Cooperatively scheduled.** A guest runs until it yields or completes. There
  is no preemption.
- **Orthogonally persistent.** A process has no filesystem. The runtime rebuilds
  its state by replaying a journal of its syscalls (lineage: AS/400, KeyKOS).
- **Durably replayable.** The journal is both the crash-recovery log and the
  audit trail (lineage: Temporal, ReVirt).

The word "kernel" names only this role in the model. The component is the
**processor**. No package, type or file uses the name "kernel".

## Glossary

A name takes an OS term only when it keeps that term's contract. So there is no
`Thread` (no preemption) and no `Interrupt` (yields are cooperative).

| Code | OS concept | Contract |
|---|---|---|
| Wasm module | Program image | Code. Many processes can run one program. |
| `Program`, `NewProgram` | Loaded program | One compiled image. It creates processes. |
| `NewProcess` | `posix_spawn` | Input, credential, dispatcher and quantum deadline are explicit. The process inherits nothing. |
| `Process` | Process | One running instance. |
| `Resume`, `ResumeHandle`, `ResumeResult` | Schedule a quantum | The process runs until it yields, completes, stops or fails. |
| `sys.Syscall` | Syscall request | The guest-to-host request. |
| `sys.SyscallResult` | Syscall result | `result`, `yield` or `failed`. |
| `sys.Authorization` | Approval context | The authority that a human decision gave to one call. |
| `Process.Cred` | Credential | Host-side identity. The guest never sees or supplies it. |
| `sys.Dispatcher` | Driver interface | Turns a `Syscall` into a `SyscallResult`. What a syscall means is the runtime's concern. |
| aurora-dispatchers drivers | Drivers (outbound) | Do a process's I/O with the outside world. |
| Chat connectors (Slack) | Drivers (inbound), controlling terminal | Start and drive processes from outside. |
| `journaled.Record` | Journal (write-ahead log) | Append-only intent and completion records. |
| `Validator`, `Flow`, `Taints` (`monitor`) | Reference monitor | Checks every call against the grant set, the argument schema and the flow policy. |
| `monitor.Wrap` | The canonical chain | Puts each layer on its side of the replay boundary, in code. |
| `pool` (`internal/agent`) | Scheduler | A fixed worker pool, first come first served, with a deadline per quantum. |
| `core.scratch` | Session working directory | A durable key-value store that the processes of one session share. |

The syscall vocabulary is in package `sys`, because `syscall` would shadow the
Go standard library.

## The syscall ABI

The guest-host boundary is one Extism host function: namespace
`extism:host/compute`, function `syscall`.

```
Syscall:   {"abi":4, "name":"mail.send", "args":{…}}
Response:  {"status":"result"|"yield"|"failed", "code":"<errno>",
            "result":{…}, "message":"…", "labels":[…]}
```

The envelope is the JSON form of `sys.Syscall` and `sys.SyscallResult`. The
host decodes a `sys.Syscall`, gives that value to the dispatcher chain, and
encodes the result. The journal uses the same encoding. `args` and `result`
are opaque payloads inside the envelope. ABI v3 used protobuf. v4 removed it,
because typed payloads (not a typed envelope) would have given its benefits,
at the cost of code generation inside every guest.

The host refuses a different ABI version with errno `bad_abi`. A failure
carries an errno from a closed set, so a guest does not parse messages. The
guest returns `{"status":"completed"}` or `{"status":"yielded"}` from its
entrypoint. Treat this ABI like POSIX: keep it small and version every change.

**Redo scopes.** `sys.begin` and `sys.commit` bracket a section. They are
journaled markers with no side effect. A section can only be run again, never
undone. So a section around effects that are not idempotent can repeat those
effects. `sys.abort` abandons the open section: the runtime forks the journal at
its `sys.begin` and runs the section again, at most 10 times. With no open
section, `sys.abort` fails the process. There is no undo layer.

**The triad.** Every dispatch carries three values:

- `cred`: who asks (host-side, never visible to the guest).
- `syscall`: what is asked.
- `auth`: what a human approved for this call.

Leaf drivers do work and ignore `cred`. Only policy layers (validation,
approval, flow) read it. A leaf driver that makes an authority decision from
`cred` is in the wrong layer.

## The five laws

1. **No ambient authority.** A process can only call granted capabilities.
   *Code:* `NewProgram` refuses images with `allowed_hosts` or `allowed_paths`
   (`ErrAmbientAuthority`). The processor builds the guest module config. The
   caller cannot supply it (`ambient.go`).
2. **Determinism.** All non-determinism (clock, randomness, I/O) goes through
   syscalls. *Code:* the WASI clock and random source are deterministic and
   restart the same way for each instance (`ambient.go`). Real time and
   entropy are the journaled `sys.now` and `sys.random`.
3. **Journal write-ahead.**
   - *Journal before execute:* an intent record is appended before the driver
     runs. A crash after the effect leaves an open intent.
   - *Journal before observe:* the completion record is appended before the
     guest sees the result, so replay is exact.

   A yield is never committed. Its intent stays open while the task is pending.
   *Code:* the intent/completion tape (`journaled`). Replay runs an open intent
   again, so effects are at least once in the crash window. Drivers get no
   idempotency key. `journaled.Verify` checks the record structure. The journal
   is not tamper-evident.
4. **Complete mediation.** The monitor checks every call before a driver sees
   it:
   - A name outside the grant set gets `denied`.
   - Arguments that fail the operation's schema get `invalid_args`.
   - A capability that requires approval does not run without an approved
     `Authorization`.
   - **Information flow:** each operation declares the source labels its
     results carry (`labels`, for example `untrusted_web`) and the labels that
     must not flow into it (`taints`). The guest is opaque, so every label a
     process observes taints everything it does after.
   - **Declassification** is the `sys.declassify` syscall. It names the labels
     and a reason, needs a human approval every time, and is journaled, so
     replay applies it again without a new question. It must be granted like
     any capability.

   *Code:* the chain for one process, outermost first:

   ```
   Validator → Flow.Guard → replay → Flow.Stamp          (monitor.Wrap)
     → savepoints → world → lifecycle → task → Declassifier → progress → timer → drivers
   ```

   Layers above replay (validation, flow denials) are derived again on every
   pass and never journaled. Layers below replay are journaled once. The task
   layer turns a yield from beneath it into a durable approval task.
   `monitor.Wrap` builds the top of the chain, so a layer on the wrong side of
   the replay boundary cannot be expressed. The monitor also passes the
   process's taint down (`capability.WithTaint`), so a driver that stores data
   keeps its labels.
5. **Minimal TCB.** The processor owns lifecycle, syscall dispatch and
   isolation. Nothing else goes in it.

## Drivers

Drivers connect processes to the outside world, in two directions:

- **Outbound drivers** are dispatchers. A process calls them: `core.internet`,
  `core.httpTemplate`, `core.command`, `core.openaiApi`, `core.scratch`,
  `sys.timer`.
- **Inbound drivers** are connectors. They drive processes from outside: the
  Slack connector, `aurora-cli`. A connector is the process's controlling
  terminal. Human approval is terminal I/O: the process yields, and the human's
  answer resumes it.

### Host commands (`core.command`)

The manifest author owns the whole command line. The guest only fills declared
slots. Aurora enforces:

- an absolute executable, with no `PATH` lookup and no shell.
- slot values from a closed set or an anchored pattern, never starting with
  `-`, never with control characters.
- an absolute working directory, stated in the manifest.
- an environment that the manifest declares, with nothing inherited.
- approval (on by default), with the resolved command line in the prompt.
- the flow policy of the command's entry.

Aurora does **not** confine what the binary does after it starts. The binary
runs with the server's operating-system rights. The author vouches for every
allowed command and slot value. Some binaries read meaning into a value with
no dash (`curl -d @file` uploads a local file). Confinement comes from the
deployment (a dedicated user, a container, egress rules) and from
least-privilege credentials in the command's environment. The ROADMAP parks a
per-command OS sandbox.

## Scheduling

Scheduling is runtime policy. The processor gives only `Resume`, one quantum.
The scheduler is `internal/agent/pool.go` in aurora-capcompute: a fixed worker
pool, first come first served, with a wall-clock deadline per quantum. Every
quantum starts a fresh instance.

## Persistence, replay and program versions

A process is durable because the runtime journals every syscall outcome. After
a crash, the runtime creates the process again from its program and input, and
replays the journal to the point of interruption. The same journal is the
audit trail.

Replay requires the same program. A program change that adds, removes,
reorders or changes syscalls makes the journal diverge. So a process is bound
to the digest of its program (wasm and both schemas) for life. A process whose
program changed cannot resume. The runtime refuses it with a conflict. To
change a program, drain or stop its processes, then restart with the new
program. New processes run the new program. Journal migration and snapshots
are not built.

## Parked designs

- **Child processes (`spawn`).** Built, then removed with no consumer. If they
  return, use `spawn`, not `fork`: a fresh process with explicit capabilities.
  The child's grants are a subset of the parent's. The child id is
  deterministic. The parent waits for the child's result, and replay reads it
  from the child's journal.
- **Cross-session memory.** Today `core.scratch` is per session. A store that
  outlives sessions must be a journaled capability (determinism: replay reads
  the recorded value) with explicit grants (no ambient authority). Values keep
  the writer's labels, so a planted instruction comes back as untrusted.
  Concurrent writers from different sessions will need compare-and-set.

## Non-goals

Build an OS concept only when a real requirement needs it. Not planned:
preemptive scheduling, virtual memory, a POSIX filesystem, signals other than
cancel, and asynchronous IPC between processes.
