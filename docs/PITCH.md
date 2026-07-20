# How to explain Aurora (without the cringe)

A field guide for conversations with experienced engineers. The rule that
governs everything here: **lead with the failure story, never the metaphor.**
The OS frame is an internal design discipline (see `ARCHITECTURE.md` — it keeps
finding real bugs); as an *opening line* it triggers the crackpot prior and the
listener disengages before reaching substance. When you claim the metaphor, you
carry the burden of proof. When *they* discover it ("huh, that's basically an
OS"), it lands as insight. Same content, opposite reception.

Also remember what fame actually buys: not belief — *patience*. The substitute
available to an unknown builder is the demo. Nobody argues with a `kill -9`.

---

## The opening (30 seconds, problem first)

> "Agents that take real actions — kubectl, payments, prod changes. Three
> problems nobody's stack answers:
> **One** — the agent just ran twelve actions against prod. Show me the record.
> Not stdout it chose to print — a record it *couldn't* skip or forge.
> **Two** — it died at step 7 of 12, and step 3 was a payment. Restart it. What
> happens? Steps 1–6 run again. You just paid twice.
> **Three** — where does 'a human must approve this specific delete' live? In
> the agent's own prompt — which the LLM controls. The prisoner writes the
> prison rules.
> I built the runtime where those three have answers: every side effect goes
> through one journaled gate, crashes replay to the exact instruction without
> re-running committed effects, and approval is enforced outside the sandbox
> where the agent can't decline it."

Stop there. Let them ask. Every clause is made of ideas they already respect
(Temporal, gVisor, capability security) without naming any of them.

## The 90-second demo (beats any pitch)

The aurora-k8s-agent assembly, live:

1. Telegram: ask the bot for a k8s change → **approval card** appears
   (operation, args, expiry).
2. Approve → run proceeds; status message updates per action.
3. Mid-run: **`kill -9` the agent process.** On stage. Say nothing.
4. Restart it → the run **resumes at the exact step**; no action repeats.
5. `/journal` → every syscall, the approval, the actor, in order.

Then one sentence: "The journal you're looking at is also the mechanism that
resumed the run — durability and audit are the same log." That's the whole
architecture, demonstrated instead of claimed.

(Stack note, 2026-07: the chat-card assembly above is the deprecated
aurora-k8s-agent; the surviving stack runs the same beats headless —
`aurora-cli` shows the approval task, `kill -9` the `aurora-dist` binary,
restart, the process resumes; verified end-to-end including a restart
mid-timer-wait. Chat cards return with the first connector, D3.)

---

## Objection kit (steelmen, then answers)

### "Why not just run agents in Docker?"

The most common one, and it's a *category* confusion — answer with the axis
split, not with features:

> "Docker protects your **host from the agent**. Nothing in Docker protects
> your **production from what the agent does with the credentials you handed
> it**. Isolation and governance are different axes; I'm on the second one."

Then, if they engage, the three questions from the opening, sharpened:

1. **Ambient authority.** A container with a mounted kubeconfig gives the agent
   *everything that credential allows, any time, no per-action gate, no
   witness*. Aurora inverts it: the guest starts with zero authority; every
   action crosses one gate that can log, require approval, or deny — per call.
2. **Crash-resume.** You cannot checkpoint-restore a Docker process into an
   exact resume, because the code is nondeterministic (LLM calls). A
   deterministic wasm guest + a journal of syscall results replays to the exact
   instruction, and committed effects are not re-executed. This — not security
   fashion — is *why wasm and not a container*.
3. **Enforced approval.** In Docker, human-in-the-loop lives in the agent's own
   code/prompt: cooperation, not enforcement. Aurora's gate is outside the
   sandbox; there is no code path around it.

Closer: even the big labs run computer-use agents in VMs *and still* have the
approval/audit/injection problems — that's why Google published CaMeL (2025).
Isolation is table stakes. Aurora is the layer that's missing *after* isolation.

### "Why not just use Temporal?"

Concede first: "Temporal is the right answer for durable *trusted* code — I
share its replay model." Then the delta:

- Temporal executes **your trusted workflow code** with the full process
  authority. Aurora executes **untrusted, LLM-steered code** in a sandbox with
  zero ambient authority — the threat model Temporal doesn't have.
- Capability grants, per-call human approval, and argument/flow policy are
  processor primitives here; on Temporal you'd hand-build all three inside trusted
  code the agent influences.
- Same lineage, different trust boundary: "Temporal for code you trust; this
  for code you can't."

### "A syscall is small and fast; yours are big and slow. And this isn't a real OS."

Two-part answer. On *syscall*: the definition is the mediated gate, not the
latency. Counterexamples they use daily: a `read()` on **FUSE** is serviced by
a userspace daemon that may hit S3 for seconds — still a syscall; **NFS** makes
`read()` an RPC to another machine; **gVisor** services intercepted syscalls in
a userspace Go process Google calls "an application kernel"; and **WASI** — the
substrate this runs on — literally standardized the guest→host boundary as a
*System Interface*. On *real OS*: by the manages-hardware definition, correct —
and our own docs say so. By that same definition **MirageOS** (whose actual
name for itself is "library operating system"), unikernels, exokernel libOSes
(SOSP '95), and User-Mode Linux also aren't OSes. "Library OS" is a
thirty-year-old term of art, not a vanity claim. Two-word version: *gVisor,
MirageOS*.

(But note: if the conversation is here, it derailed — this is the weakest
objection and usually means they disengaged at the framing. Steer back to the
demo.)

### "JSON isn't an ABI. Why not WIT/component model or gRPC?"

The ADR (CHALLENGE.md E): the uniform envelope is deliberate — it's what makes
one-chokepoint mediation work. Linux syscalls are uniform (number + registers),
and that uniformity is why `strace`/`seccomp`/audit can interpose generically;
per-interface typed contracts (WIT) optimize app ergonomics at the cost of
generic interposition — and wazero (the pure-Go runtime) has no component-model
support at all. The *encoding* of that envelope is JSON (ABI v4): protobuf was
tried as v3 and withdrawn, because its payoff needed typed args rather than a
typed envelope, and the envelope alone cost a hand-rolled codec in every guest
language.

### "Coarse syscalls must be slow."

Cadence is LLM-turn-scale: a handful of syscalls per turn against seconds of
model latency. The boundary is an in-process memory copy costing microseconds —
statistically zero. Fine-grained hot-path syscalls are not this workload.

### "The LLM can still be prompt-injected into misusing granted tools."

Concede the premise, then show the shipped layer (CHALLENGE.md A, all four
stages): capability gating alone doesn't stop a granted tool being used with
tainted data — so results carry provenance labels, the gate enforces flow
policy (tainted data cannot reach a protected capability), declassification is
an explicit human-approved syscall, and the plan/execute (dual-LLM) camel
program quarantines untrusted tool output from planning. This is the CaMeL
architecture as processor primitives — the deterministic mediator CaMeL needs is
what the dispatcher already is. Stay honest about the residual: flow policy
bounds where tainted data can *go*; inside those bounds a fooled model can
still act badly — which is what approval gates and the audit trail are for.

---

## Audience calibration

- **This pitch is wasted on engineers who've never shipped an agent somewhere
  regulated.** The pain is theoretical to them; expect "why not Docker" and
  treat it as the standard immune response every new layer gets (Dropbox got
  "just use rsync"; Docker got "just use LXC"). Don't seek validation there.
- **The right audience** has hit the wall: tried to deploy an action-taking
  agent and got blocked by security/compliance, or got paged for one. Their
  first question isn't "why not Docker" — it's "the approval is *enforced*?
  Show me."
- **Vocabulary switching**: to engineers say "enforced approval, exact-replay,
  un-forgeable audit log"; to security/compliance say "governance, complete
  mediation, audit trail"; keep "OS/kernel/syscall" for design docs and for
  people who discover the mapping themselves — then share ARCHITECTURE.md and
  let the coherence do the talking.
- **Let the artifact be famous instead of you**: working demo, readable code,
  and docs that show decade-deep homework (RESEARCH.md/CHALLENGE.md) are the
  unknown builder's substitute for a name.
