package capcompute

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/aurora-capcompute/capcompute/sys"
	"github.com/aurora-capcompute/capcompute/sys/replay/tape/journaled"
)

// ErrUnwindYielded means the in-flight compensation yielded — typically
// awaiting a human approval. The compensation intent stays open in the
// journal; call Unwind again once the external task resolves and it resumes
// under the same idempotency key.
var ErrUnwindYielded = errors.New("unwind yielded: a compensation awaits an external task")

// CompensationAction says what unwinding did about one effect.
type CompensationAction string

const (
	// CompensationSkipped — the capability declares CompensateNone; nothing to undo.
	CompensationSkipped CompensationAction = "skipped"
	// CompensationDispatched — the declared inverse ran and returned a result.
	CompensationDispatched CompensationAction = "compensated"
	// CompensationEscalated — no mechanical undo exists (undeclared,
	// CompensateEscalate, a vanished capability, or a failed inverse): a
	// human resolves it with the journal — the terminal compensator.
	CompensationEscalated CompensationAction = "escalated"
)

// CompensationOutcome reports what unwinding did about the effect journaled
// at position Compensates.
type CompensationOutcome struct {
	Compensates int
	Original    sys.Syscall
	Action      CompensationAction
	Inverse     sys.Syscall       // set when Action is CompensationDispatched or the inverse failed
	Result      sys.SyscallResult // the inverse's result when one was dispatched
	Reason      string            // why an escalation escalated
}

// CompensationArgs is the args envelope every inverse capability receives:
// which effect it undoes, the original syscall, and the original result.
type CompensationArgs struct {
	Compensates int               `json:"compensates"`
	Syscall     sys.Syscall       `json:"syscall"`
	Result      sys.SyscallResult `json:"result"`
}

// Unwind compensates the completed effects journaled at position >= from,
// newest first (the saga order). For each effect it consults the capability's
// declared Compensation: reads are skipped, declared inverses are dispatched
// through the supplied dispatcher (journaled as compensation records, each
// carrying an idempotency key, composable with approval via yield), and
// everything else escalates. Unwinding is crash-resumable: an interrupted run
// leaves an open compensation intent that a later Unwind resumes under the
// original key.
//
// Unwinding a whole aborted run is Unwind(..., 0, ...); unwinding one scope
// passes the scope's first journal position.
func Unwind[K any](
	ctx context.Context,
	cred K,
	journal journaled.Journal,
	from int,
	dispatcher sys.Dispatcher[K],
) ([]CompensationOutcome, error) {
	compensator, err := journaled.NewCompensator(journal)
	if err != nil {
		return nil, err
	}
	effects, resume, err := compensator.Effects(from)
	if err != nil {
		return nil, err
	}

	declared := make(map[string]sys.Capability)
	for _, capability := range dispatcher.Capabilities() {
		declared[capability.Name] = capability
	}

	var outcomes []CompensationOutcome
	if resume != nil {
		outcome, err := dispatchCompensation(ctx, cred, dispatcher, compensator, resume.Syscall, resume.Compensates, resume.Key, true)
		outcomes = append(outcomes, outcome)
		if err != nil {
			return outcomes, err
		}
	}

	for _, effect := range effects {
		capability, known := declared[effect.Syscall.Name]
		compensation := capability.Compensation

		switch {
		case !known:
			outcomes = append(outcomes, CompensationOutcome{
				Compensates: effect.Position,
				Original:    effect.Syscall,
				Action:      CompensationEscalated,
				Reason:      fmt.Sprintf("capability %q is no longer granted", effect.Syscall.Name),
			})

		case compensation.Kind == sys.CompensateNone:
			outcomes = append(outcomes, CompensationOutcome{
				Compensates: effect.Position,
				Original:    effect.Syscall,
				Action:      CompensationSkipped,
			})

		case compensation.Kind == sys.CompensateSyscall && compensation.Syscall != "":
			args, err := json.Marshal(CompensationArgs{
				Compensates: effect.Position,
				Syscall:     effect.Syscall,
				Result:      effect.Result,
			})
			if err != nil {
				return outcomes, err
			}
			inverse := sys.Syscall{Abi: sys.ABIVersion, Name: compensation.Syscall, Args: args}
			key, err := compensator.Begin(inverse, effect.Position)
			if err != nil {
				return outcomes, err
			}
			outcome, err := dispatchCompensation(ctx, cred, dispatcher, compensator, inverse, effect.Position, key, false)
			outcome.Original = effect.Syscall
			outcomes = append(outcomes, outcome)
			if err != nil {
				return outcomes, err
			}

		default: // CompensateEscalate, undeclared, or a malformed declaration
			outcomes = append(outcomes, CompensationOutcome{
				Compensates: effect.Position,
				Original:    effect.Syscall,
				Action:      CompensationEscalated,
				Reason:      fmt.Sprintf("capability %q declares no mechanical undo", effect.Syscall.Name),
			})
		}
	}
	return outcomes, nil
}

// dispatchCompensation runs one inverse whose compensation intent is already
// journaled and commits its outcome. On yield the intent stays open and
// ErrUnwindYielded is returned; on a Go error the intent stays open for a
// later resume; on a failed result the outcome escalates.
func dispatchCompensation[K any](
	ctx context.Context,
	cred K,
	dispatcher sys.Dispatcher[K],
	compensator *journaled.Compensator,
	inverse sys.Syscall,
	compensates int,
	key string,
	resumed bool,
) (CompensationOutcome, error) {
	outcome := CompensationOutcome{Compensates: compensates, Inverse: inverse}

	result, err := dispatcher.Dispatch(sys.WithIdempotencyKey(ctx, key), cred, inverse, sys.Authorization{})
	if err != nil {
		outcome.Action = CompensationEscalated
		outcome.Reason = fmt.Sprintf("compensation dispatch error: %v", err)
		return outcome, err
	}
	if result.Status() == sys.StatusYield {
		outcome.Action = CompensationDispatched
		outcome.Result = result
		return outcome, ErrUnwindYielded
	}
	if err := compensator.Commit(result); err != nil {
		outcome.Action = CompensationEscalated
		outcome.Reason = fmt.Sprintf("commit compensation: %v", err)
		return outcome, err
	}

	outcome.Result = result
	if result.Status() == sys.StatusFailed {
		outcome.Action = CompensationEscalated
		outcome.Reason = "the inverse syscall failed"
		if resumed {
			outcome.Reason = "the resumed inverse syscall failed"
		}
		return outcome, nil
	}
	outcome.Action = CompensationDispatched
	return outcome, nil
}
