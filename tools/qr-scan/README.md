# qr-scan

Decodes the QR codes `internal/qr` draws, at several simulated viewing
distances, so the size of the logo in the middle of them is a **measurement**
rather than an argument.

```sh
make verify-scan                                  # the gate
make verify-scan SCAN_ARGS="--ppm 4,3,2"          # closer or further away
make verify-scan SCAN_ARGS=--zbar                 # plus a third engine, reporting only
```

## Why it exists

[M50.6](../../docs/build-notes/phase-details/m50.6.md) puts a logo in the middle
of a code, which destroys modules; error correction is the only reason that is
survivable, and `internal/qr/composite.go` derives from level H's budget how
much of a code the logo may cover. As shipped that was a **fifth** of the
symbol's width, and the owner asked for it "as big as possible without making
the barcode unreadable" ([F215](../../docs/build-notes/deferred-findings.md)).

The reopened milestone answers *how big* by measuring, and requires the
measurement to be **kept rather than run once**, because a number nothing
re-checks is a number the next change quietly invalidates. This is that check.
It is what says the cap is **three tenths** of the symbol's width and not a
third — the third fails, here, reproducibly.

## What it does

Two halves, because Go has no QR decoder and adding one to `go.mod` would break
[M49](../../docs/build-notes/phase-details/m49.md)'s assertion that the QR path
adds no dependency. Node is admissible for verification tooling under
[D25](../../Plan.md), the way `../render-verify` and `../agent-browser` already
are; nothing here is imported by the product, built into it, or in the image.

1. **`internal/qr`'s `TestWriteScanCorpus`** renders the corpus, off the
   shipping path — `qr.RenderPNGWithLogo`, the same call the download endpoint
   makes — into a directory `make verify-scan` creates and removes. **1360
   pictures**, in two halves of 680:

   - every symbol version the product's content lengths reach (3 to 36), four
     logo shapes, at five *stored* sizes each: the smallest, the default and the
     largest, and — since M49's second reopening — two more carrying the
     **band's low end**, a three-module quiet zone at both ends of the scale
     range. Three modules is one below what ISO/IEC 18004 specifies and is what
     `qr.FitSize` may now produce, so it is measured here rather than argued;
   - the **whole** version range again with no logo at all, at every level, as
     the control. The whole range and not a sample of it, because what the
     control is for is to say that a decoder's misses are the logo's doing and
     not its own limit — and it cannot say that about a version it does not
     cover. It was three versions until M50.6's reopening was reviewed, where
     the versions the third engine missed turned out to be mostly outside them.
     A control payload is sized for its version at level H, so at the other
     three levels it encodes to a smaller symbol and the control's range runs
     from 2 rather than 3.

   It writes a `manifest.tsv` saying what each picture should decode to.
2. **`scan.mjs`** shrinks each picture so that 8, 6, 4, 3 and then 2 pixels per
   module are left — averaging over the source area each destination pixel
   covers, which is what a sensor does — and decodes every one through two
   decoders. A picture that fails to come back byte-identical through either is
   a non-zero exit.

## The decoders, and why these two

| | |
| --- | --- |
| **zxing-wasm** | `zxing-cpp` compiled to WebAssembly — the algorithm most commercial scanners descend from. Robust: it read every fraction in the sweep, at every distance. |
| **jsQR** | A separate implementation with its own detector. Stricter about the thing that matters here: it is what fails at a third of the symbol's width, at version 13, for every logo shape and at every distance |

Both must read every picture. A fraction only one of them reads is a fraction
that depends on a decoder, which is the thing measuring was supposed to avoid.

Two more were considered and are not gates:

- **`@zxing/library`**, the JavaScript port rather than the wasm build, fails
  *plain unoccluded* codes at versions 14, 16, 20 and 34. A decoder that cannot
  read a code with no logo in it cannot be evidence about one that has.
- **`zbarimg`** is a system package, so it cannot be pinned in `package.json`
  and CI has no such binary. `--zbar` runs it when it is on `PATH` and
  **reports without gating**. It is the strictest engine to hand and its dissent
  is recorded rather than hidden — 0.23.93, at the same five distances, one
  decode each. The figures below are the 816-picture corpus as it stood at
  M50.6's reopening; the corpus is 1360 pictures since M49's second one, and the
  re-run is in that milestone's decision entry:

  | | Logo'd | Control |
  | --- | --- | --- |
  | One fifth, as shipped before the reopening | 1484 of 1496 | 1496 of 1496 |
  | **Three tenths, as shipped now** | **1386 of 1496** | **1496 of 1496** |

  against 5984 of 5984 for the two gating decoders at either fraction. **The
  control is why those numbers can be read as being about the logo**: it is
  clean at both, over the whole version range, so the misses are the occlusion's
  doing and not the engine's own limit. They concentrate at the aggressive end
  of the distance simulation rather than at any one stored size — 84 of the 110
  are read at two pixels per module, as are all twelve of the old fifth's.

## What it does not prove

**A downscaled picture is not a poster.** Shrinking approximates the one thing
that dominates at distance — a camera with fewer pixels per module to work
with — and none of the rest: ink spread, glare, paper, camera noise, a curved
surface. So a pass is evidence of *relative* safety between two candidate
fractions, which is exactly what choosing between them needs, and not a promise
about any particular print. The budget arithmetic in `composite.go` is what
bounds the worst case; this is the evidence that the bound is not the only thing
standing between a logo and an unreadable code.

It is also **not part of `make check` or of CI**, for `verify-render`'s reason:
it needs Node, and the shipped build does not.
