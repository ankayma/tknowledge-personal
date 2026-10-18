# Template: job-search

`template.json` lists the files below. A template need not be complete: a missing or empty file makes `setup_templates` report `complete: false`, and `setup_apply` applies what is there and returns the missing titles for the user to write during setup.

| File | Content |
|---|---|
| `principle-list.md` | The principle list (P.1–P.5, P.8, P.9) |
| `rules-comment-connect-filter.md` | Sample rules for comments, LinkedIn posts and connection requests |
| `instructions-job-search.md` | Project instructions to paste into the assistant |

To build your own template from a store set up over MCP: `tk org template export --id <id> --out <dir>`, then point tk-api's `TK_TEMPLATES_DIR` at the directory. Files added here are embedded into tk-api when it is built. After changing the content of a file already applied, raise `version` in `template.json` so the basis `template:job-search@<version>` tells the versions apart.
