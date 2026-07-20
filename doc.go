// Package capcompute is the processor: it runs Extism wasm programs as
// processes whose only access to the outside world is host-dispatched
// syscalls.
//
// A note on vocabulary, because two words are in play. The component is the
// **processor** — that is what this package is, and the name the code uses
// throughout. "Kernel" appears in the design docs as the processor's *role*
// in the library-operating-system model (docs/ARCHITECTURE.md), alongside
// programs, processes, and syscalls. It names no package, type, or file here.
//
// Three calls:
//   - NewProgram compiles a program image, rejecting any ambient authority
//     (no allowed hosts or paths; clock, RNG, and env are pinned by the
//     processor, never caller-supplied);
//   - NewProcess instantiates a process from a program — posix_spawn semantics:
//     explicit input, an explicit credential, an explicitly handed syscall
//     dispatcher, nothing inherited;
//   - Resume gives a process the CPU for one cooperative quantum, until it
//     completes, yields, fails, or is stopped.
//
// Resume plants a dispatch closure — already bound to the process's
// credential and dispatcher — in the call context, so the syscall host
// function serves exactly the process that was given the CPU and needs no
// lookup of its own.
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
