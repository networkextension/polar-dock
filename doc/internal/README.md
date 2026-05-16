# doc/internal/

Working-set research notes that informed published designs, kept in
the repo for provenance but **not** authoritative spec material. The
files in here are LLM-conversation logs, scratch drafts, source
research, etc.

What's in here:

- `jtag-research/` — research notes that fed `doc/jtag-debug-skill-design.md`.
  Discusses vendor-specific debug tooling (probe controllers, USB-C
  debug cables, JTAG-over-USB libraries) at a depth that's irrelevant
  to the published design but useful when you need to recall *why* a
  design choice was made.

Vendor-specific product names that are not industry-standard have
been globally rewritten to vendor-neutral equivalents (e.g. JTAG
probe / JTAG library / vendor JTAG controller). When you read these
notes, treat the rewrites as the source of truth — the underlying
implementation can target whatever vendor's probe the operator
happens to have.
