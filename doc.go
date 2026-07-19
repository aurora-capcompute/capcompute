// Package capcompute is the kernel of a small library operating system for
// Extism compute guests: wasm programs run as processes whose only access to
// the outside world is host-dispatched syscalls.
//
// The package is a processor — three calls:
//   - NewProgram compiles a program image, rejecting any ambient authority
//     (no allowed hosts or paths; clock, RNG, and env are pinned by the
//     processor, never caller-supplied);
//   - NewProcess instantiates a process from a program — posix_spawn semantics:
//     explicit input, an explicit credential, an explicitly handed syscall
//     dispatcher, nothing inherited;
//   - Resume gives a process the CPU for one cooperative quantum, until it
//     completes, yields, fails, or is stopped.
//
// Resume plants the process in the call context, so the syscall host
// function dispatches through exactly the process that was given the CPU —
// there is no other lookup.
//
// Resume has four observable outcomes. A successful guest call whose JSON
// output contains {"status":"yielded"} returns ResumeYielded, while an explicit
// {"status":"completed"} returns ResumeCompleted. Missing or unsupported status
// values return ResumeFailed. A stopped invocation returns ResumeStopped and
// permanently terminates that physical process. Guest and runtime errors also
// return ResumeFailed.
//
// The library does not own process lookup, replay scheduling, async
// completion, durable journal policy, or process cleanup timing. Those are
// conventions of the wrapping system using this package.
package capcompute
