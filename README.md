# autogit

autogit is a small daemon that keeps git repositories in sync with their
remotes: notes, journals, dotfiles and other repositories that you edit on
several machines and never want to commit by hand.

For every configured repository it

- **commits** changes in the worktree after a quiet period,
- **pushes** new local commits, made by autogit or by you, to the remotes
  you allow,
- **pulls** upstream changes when that is safe: a fast-forward, or a
  rebase of autogit's own unpublished commits.

Whenever a human is needed (a conflict, diverged history with your own
commits, a merge or rebase in progress, a detached HEAD), autogit forces
nothing. It pauses pull and push for that repository, reports why, and
resumes on its own once the situation is resolved. It never force-pushes,
resets or stashes.

## How it works

```
 worktree ──fsnotify──► debounce ──► git add -A; git commit ──┐
                                                              ▼
 .git/HEAD, refs/heads ──fsnotify──────────────────────────► push to allowed remotes
                                                              ▲
 every pull_interval ──► fetch ──► fast-forward / rebase ─────┘
```

- **Worktree watcher.** Watches the worktree recursively, skipping `.git`,
  nested repositories and directories ignored by git. Events are filtered
  through `git check-ignore` and the `ignore` file name patterns (editor
  lock and backup files by default). After `debounce` without further
  changes, and at most every `min_commit_interval`, autogit stages
  everything and commits with the configured message template. Each
  commit carries a trailer `Autogit: <host>`.
- **Refs watcher.** Watches `HEAD`, `packed-refs` and `refs/heads/`. When
  the current branch moves, for whatever reason, autogit pushes it to
  every remote in `push_remotes` that already has this branch. New
  branches are never created on remotes.
- **Sync** (every `pull_interval`, on start and on `autogit sync`):
  1. Pause if a merge, rebase, cherry-pick, revert or bisect is in
     progress, or HEAD is detached.
  2. Skip the pull if tracked files have uncommitted changes (with
     `auto_commit`, they are committed first).
  3. Fetch the upstream remote and the push remotes.
  4. Behind only: `merge --ff-only`. Diverged: rebase onto the upstream
     if every local commit has the autogit trailer and none of them was
     pushed to another remote; otherwise pause. A failed rebase is
     aborted and the repository paused, with the worktree as it was.
  5. Push.
- If a push is rejected because the remote moved, autogit syncs once and
  retries; if it is still rejected, the repository is paused.
- Network errors put the repository in the `error` state; it is retried
  on the next sync.

Pull from the branch's upstream (`branch.<name>.remote`), push to the
allowlist. With `push_remotes: [backup]` and an upstream on `origin`,
autogit pulls from `origin` and pushes to `backup` only.

## Configuration

`~/.config/autogit/config.yaml` (or `$XDG_CONFIG_HOME/autogit/config.yaml`,
or `-config <file>`). See [contrib/config.example.yaml](contrib/config.example.yaml).

```yaml
defaults:
  push_remotes: [origin]
  debounce: 5s
repos:
  - path: ~/notes
  - path: ~/journal
    push_remotes: [backup]
    commit_message: "journal: {{.Count}} files on {{.Host}}"
```

| Option | Default | |
|---|---|---|
| `auto_commit` | `true` | commit worktree changes |
| `auto_push` | `true` | push new commits to `push_remotes` |
| `auto_pull` | `true` | fetch and integrate the upstream |
| `push_remotes` | `[]` | remotes autogit may push to |
| `commit_message` | `autogit: {{.Files}} on {{.Host}}` | Go template |
| `sign` | `false` | sign autogit commits |
| `debounce` | `5s` | quiet time before committing |
| `min_commit_interval` | `1m` | minimum time between autogit commits |
| `pull_interval` | `2m` | how often to fetch |
| `ignore` | editor lock/backup files | file name globs never committed |
| `on_pause` | | shell command run when a repository pauses |

Per repository there are also `path` (required) and `name` (defaults to the
directory name). Top-level options: `state_file` (default
`~/.local/state/autogit/status.json`) and `git_timeout` (default `2m`).

Template fields: `.Files` (changed paths, shortened to "a, b and 3 more"),
`.Count`, `.Host`, `.Repo`, `.Branch`, `.Time`.

`on_pause` runs with `AUTOGIT_REPO`, `AUTOGIT_PATH` and `AUTOGIT_REASON`
in its environment, once per new pause reason.

## Usage

```
autogit run             # the daemon; SIGHUP reloads the config
autogit status [-json]  # state of all repositories
autogit check [repo…]   # commit, pull and push once, without the daemon
autogit sync            # make the running daemon sync now (SIGUSR1)
```

```
$ autogit status
REPO     STATE   BRANCH  UPSTREAM     ↑  ↓  SYNCED  REASON
notes    ok      main    origin/main  0  0  12s ago
journal  paused  main    origin/main  1  2  12s ago diverged from origin/main (1 local, 2 remote): local commit 3f2a9c1e0b was not made by autogit
```

States: `ok`, `dirty` (uncommitted changes with `auto_commit: false`,
pull skipped), `paused` (needs you) and `error` (retrying). `status` exits
with 1 if a repository is paused or failing or the daemon is not running.
`status -json` prints the state file for status bars and scripts.

## Installation

With Nix: `nix build github:leoc/autogit` or use the flake's
`packages.<system>.default`. Otherwise `go install github.com/leoc/autogit@latest`;
git must be in `PATH`.

Run it as a systemd user service with
[contrib/autogit.service](contrib/autogit.service). The service needs
access to your SSH agent to push over SSH.

## Development

```
nix develop
go test ./...
```

The tests create temporary repositories with a local bare remote and run
against an isolated git configuration.

## License

MIT
