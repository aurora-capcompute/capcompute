// Package capcompute is the processor: it runs Extism wasm programs as
// processes whose only access to the outside world is host-dispatched
// syscalls.
//
// The design docs call the processor's role "kernel", in the library operating
// system model (docs/ARCHITECTURE.md). In code the component is the processor.
//
// Three calls:
//   - NewProgram compiles a program image. It refuses any ambient authority:
//     no allowed hosts or paths. The processor pins the clock, the RNG and the
//     environment, and the caller cannot supply them.
//   - NewProcess makes a process from a program, with posix_spawn semantics:
//     explicit input, an explicit credential and an explicit syscall
//     dispatcher. Nothing is inherited.
//   - Resume gives a process the CPU for one cooperative quantum, until it
//     completes, yields, fails, or is stopped.
//
// The guest reports how a quantum ended in its output: {"status":"completed"}
// gives ResumeCompleted and {"status":"yielded"} gives ResumeYielded. Any other
// output, and any guest or runtime error, gives ResumeFailed. A quantum whose
// ctx ends gives ResumeStopped and terminates the process.
//
// The wrapping system owns process lookup, replay scheduling, async
// completion, journal policy and cleanup. This package owns none of them.
package capcompute
