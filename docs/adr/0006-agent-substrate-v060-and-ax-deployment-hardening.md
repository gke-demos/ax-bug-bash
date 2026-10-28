# Agent Substrate v0.6.0 and google/ax Deployment Hardening

We bake three Agent Substrate `v0.6.0` integration mitigations directly into our manifests and custom runner: (1) an OpenFGA `AccessPolicy` granting `serviceaccount:ax-system:ax-server` `admin` on `Namespace/default` (`Issue #511`), (2) storing runner state and diffs under `/workspace/.ax-state` so `SNAPSHOT_FIDELITY_VOLUMES` preserves state across suspend/resume (`Issue #518`), and (3) sub-second workspace `git clone` from the in-cluster Git server so `/readyz` passes `WakeupProbe` immediately (`Issue #482`).
