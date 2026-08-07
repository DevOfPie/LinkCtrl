# Moved — see [environment.md](environment.md)

The development environment stopped being a WSL2 distro on 2026-08-02, so this
file's name stopped describing anything. Its contents were rewritten against the
machine that exists now and live in [environment.md](environment.md).

**This pointer is kept rather than deleted because
[decisions.md](../build-notes/decisions.md) is append-only and links to this
path.** An entry there is corrected by a later entry, never by editing it, so
the reference cannot be repointed — and a dangling link fails
`make check-links`, which is a gate. One file of five lines is the cheaper half
of that trade.
