# tk — Default Instruction List (seed for CLI)

These are the **default instructions** every agent using tk receives when nothing
else is added. They are store-usage instructions (global scope). Business rules
(LinkedIn outreach, delta handoff, domain definitions) are NOT here — those are
project-scoped instructions added separately.

Term: call these **instructions**, never "rules" (to avoid confusion with
domain/business rules).

The list has two parts. **META** (prefix M) is about the instruction list itself —
how to read it and how to grow it; it applies to every instruction below. The
**numbered** instructions (1–13) are the directives an agent carries out. New
instructions added later join the numbered part and continue the numbering; META
does not grow with them.

---

## META

**M1 — How to read every instruction.** An instruction is guidance to follow, not a
hard gate. Purpose outranks wording. If an instruction would produce a result
against its purpose, surface the conflict to the owner — do not self-apply the
instruction to refuse the work.

**M2 — How to grow the list.** This list grows with practice. When a situation
recurs that no instruction covers, or an instruction produces a result against its
purpose, propose a change: state it in chat (the instruction text, the reason, the
situation that prompted it), the owner approves, then `instruction_add`. Do not add
silently. An instruction not recorded is one the next agent will not have.

---

## Group I — What to record, and under which label

**1.** Before `admit`/`assume`, list in chat the full set of claims you intend to
record (each line: the claim verbatim, its intended label, its source). Record only
the lines the owner approves.

**2.** Use [T] only for what is checkable (a document, a source, a commit).
Everything else is [A].

**3.** The status of work already done, when it has no checkable source yet, is
recorded as [A], not [T].

## Group II — How to write a claim (so it passes the gate)

**4.** One idea per claim, atomic; split joined sentences (and / semicolon) into
separate claims. This is a technical constraint of the gate, not a preference —
M1 does not override it.

**5.** A claim must be decontextualized: enough names, dates and quantities to read
it a year later apart from this conversation; no "owner", "he", "that case". Also a
gate constraint.

## Group III — Recalling and presenting

**6.** When recalling a recorded T/A in chat, quote the claim verbatim; do not
abbreviate it.

**7.** Decisive content (an instruction, a gap, a definition) appears verbatim in
chat; files and the store are the saved copy, not a place the owner must go search.

**8.** When citing an entry, state the content first and the id in parentheses after
("the first line is the conclusion (B1)"); do not write "per B1".

## Group IV — Before calling anything new

**9.** Before naming a concept or calling a mechanism new, check the store and the
documents for whether it already exists under another name.

## Group V — Summary

**10.** A summary is an authored assumption — not a claim, not evidence; every
sentence cites at least one id.

**11.** When closing a work session on a thread, overwrite the summary so the
current one stays fresh; when a summary is stale, drop it and rebuild understanding
from the T/A entries.

## Group VI — Evidence without a link

**12.** An unbound document (text only, no URL) is valid evidence — record it
without asking the owner why there is no URL. Add a one-time note that the owner
should keep the original for a future audit; do not ask again on later unbound
documents.

## Group VII — When recording costs more than it keeps

**13.** EN — Recording into this store has a cost the person pays: time, patience, and reading what the agent proposes. When that cost rises above the value of what would be kept — the person is correcting the agent repeatedly, approving lists wholesale, or hesitating to act at all — say so plainly and stop proposing: tell them that not recording is a legitimate choice, that the maintainer wants to hear exactly this (feedback_url or feedback_email from `store_info`), and that T-knowledge support can share how others handled the same situation. A store that costs more than it keeps teaches the maintainer something only if the person says so. (13/10/2026: the owner named this from his own practice — D-115.) || VI — Ghi vào kho này có một cái giá người dùng phải trả: thời gian, sự kiên nhẫn, và việc đọc những gì agent đề xuất. Khi giá đó vượt giá trị của thứ sẽ được giữ — người dùng phải sửa agent nhiều lần, duyệt cả danh sách cho xong, hoặc chần chừ không làm gì — hãy nói thẳng và ngừng đề xuất: nói với họ rằng không ghi là một lựa chọn chính đáng, rằng người bảo trì muốn nghe đúng điều này (feedback_url hoặc feedback_email từ `store_info`), và rằng hỗ trợ của T-knowledge có thể chia sẻ cách người khác đã xử lý tình huống tương tự. Một kho tốn hơn giá trị nó giữ chỉ dạy được người bảo trì điều gì nếu người dùng nói ra. (13/10/2026: chủ kho đặt tên hiện tượng này từ chính cách làm của mình — D-115.)

---

## Definitions

These are the core concepts an agent uses. Each is one line: the term, a one-sentence
definition, and the one-sentence reason it exists. The full rationale lives in the
docs (see `rationale_ref`); an agent gives the one-sentence reason in chat and points
to the docs when a user wants to go deeper. Definitions are loaded every session and
never recorded in the ledger.

**def:claim** — A statement about the world that can be checked against a source.
*Why:* checkability is what lets the ledger audit it later; a statement nobody can
check is not a claim. → `docs/claim`

**def:truth [T]** — A claim backed by a document, source, or commit; it has passed
the admission gate. *Why:* only checkable facts carry [T], so a reader trusts them
without re-auditing. → `docs/truth`

**def:assumption [A]** — A statement nobody outside could check yet: a guess, an
opinion, a spoken status, an AI judgment. *Why:* separating it from [T] keeps what is
known apart from what is supposed, which is the store's backbone. → `docs/assumption`

**def:document** — A record of evidence: the text read out of a source, with how it
was looked at (method) and, when one exists, a reference to the original (URL, file
hash). A document always needs text; the reference is optional. *Why:* the store
keeps no files, so a fact rests on the document's text — the text must stand on its
own even when no original is linked. → `docs/document`

**def:document/bound-vs-unbound** — A document is *bound* when a reference ties its
text to an original (URL + hash), *unbound* when it has only text (a screenshot, a
paper, a fact with no link). Both are valid evidence; unbound is normal, not a
defect. *Why:* an unbound document rests on its text alone, so the original is
produced by the owner only if an audit later calls for it. → `docs/document`

**def:summary** — An authored reading of where a matter stands — an assumption with
an author, never a claim and never evidence; every sentence cites an id. *Why:* it is
one person's reading, so it ranks below the facts it points to and is never used as a
source. → `docs/summary`

**def:thread** — A container that gathers everything about one matter; it carries no
label and passes no gate. *Why:* gathering is not asserting, so a thread must not be
mistaken for a claim about the matter. → `docs/thread`

**def:conversation** — A recorded exchange (a chat or message trail) linked to a
matter, kept as context. *Why:* it is raw context, not a vetted fact, so it informs
without being cited as proof. → `docs/conversation`

**def:principle** — A reasoning principle the store argues from, trusted without a
source and changed only when the owner retires it. *Why:* principles and sourced
facts are the only things trusted without audit; everything else is an assumption.
→ `docs/principle`

**def:instruction** — A behavior directive for an agent using the store, separate
from principles and from the ledger. *Why:* a directive is a command to follow, not a
fact about the world, so it lives on its own trusted path, not in search results.
→ `docs/instruction`

**def:flag** — A note that something needs a person: evidence withdrawn, a source
that changed between readings, the same source read into different text. *Why:* the
store surfaces a problem instead of silently resolving it, so trust is not lost
quietly. → `docs/flag`

**def:attester** — A person who stands behind a claim by adding their own document
for it. *Why:* a claim's strength is how many independent readers vouch for it, not
the say-so of one. → `docs/attester`
