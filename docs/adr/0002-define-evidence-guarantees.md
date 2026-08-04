# Define evidence as tamper-evident and externally witnessed

Teleop will not claim that local files are "tamper-proof." An assured **Evidence Session** requires origin authentication, hash-linked immutable admission, local crash durability, signed tree heads, and independently acknowledged **Witness Receipts**, while explicitly recording known gaps and degradation; storage retention, signer custody, trusted time, and WORM policy are supplied by deployment adapters in separate administrative and failure domains.

The externally protected claim ends at the latest acknowledged tree head. A
newer locally synchronized suffix remains a declared residual risk until a
later checkpoint is acknowledged; checkpoint policy reduces nominal lag but
cannot bound it during a witness outage. `WitnessReceipt` records adapter
success, so authenticated remote identity and durable custody remain part of
the adapter contract and deployment evidence.
