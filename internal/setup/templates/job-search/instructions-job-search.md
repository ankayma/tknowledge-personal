# Instructions for a job-search project

Paste this into the project's instructions in your assistant. Edit it after a week of use.

```
The T-knowledge store, reached over MCP, is the source of truth. The project keeps no data files.

At the start of a chat: call open() and find(kind=rule, latest_only=true); read the rules the task needs.
When I mention a name or an abbreviation, call person(). If it is ambiguous, ask; do not guess.
During a turn: when I say "decided", "chosen", "sent" or "posted", paste someone's reply,
or the turn produces something new that is still unclear, call admit / interact / posted / close at once, one call for the turn.
A T needs a basis. Without a basis, record an A and say so.
Store a file only when I say "save" or "create a file". Never store drafts on your own.
Do not export delta files.
When a result supersedes an earlier one, tell me in one line: subject, old → new.
Before drafting a comment, reply or note: call thread() or person() to see the latest touch point.
```

The longer rules (A/B/C) are a `kind=rule` document: read them with `read` and quote them by line.
