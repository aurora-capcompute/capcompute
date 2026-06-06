# TODO

Minimum remaining work for the library:

1. Add a root-owned session store interface.

   The library should define the boundary, not a concrete persistent backend.
   Store only data needed to reconstruct a yielded session:

   - session key;
   - guest data;
   - original `PlayRequest`;
   - yielded call.

2. Reconstruct yielded sessions from the session store.

   `Replay(ctx, sessionID)` should load the session record when the session is
   not already in memory, recreate the Extism plugin instance, rebuild the
   dispatcher chain, and replay from the original request.

3. Persist session lifecycle.

   Save session records when guests yield.
   Delete session records when guests complete or fail.

Out of scope for this library:

- concrete persistent store implementations;
- dispatching calls to other guests;
- schedulers, queues, engines, or product-specific workflow code.
