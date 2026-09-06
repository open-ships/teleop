# Bind command policy to the actuation transaction

An Assured Session requires a `safety.CommandPolicy`, evaluated by its Safety
Authority against frozen intent, exact input/session identity and acknowledged
baseline. Policy identity, decision and optional credential proof are retained
in the durable decision record before intent/send. A permitted decision needs
an operator identity, authorization identity and unexpired grant; grant expiry
caps the receiver lease. Low-level authorities without this adapter remain
interlock-only tools and do not claim semantic or authorization enforcement.

Safe-state transactions bypass live policy so an authorization outage cannot
trap output in its last live state. Missing policy proof or evaluation failure
inhibits and independently attempts the configured safe state. Ordinary input
supersession is distinguished from uncertainty only before any live send and
only after an acknowledged fallback; actual send uncertainty still inhibits.

The scalar reference policy and deterministic receiver model provide executable
acceptance scenarios, not vessel limits or a physical watchdog. Installed
receivers independently enforce their envelopes, session ownership and expiry;
credential custody, revocation, hardware feedback and hazard analysis remain
explicit deployment evidence. This adds a required Assured configuration field
and deliberately refuses legacy configurations lacking a command policy.
