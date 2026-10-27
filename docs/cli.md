# Command-line reference

```text
veilfold <command> [options] [arguments]
```

Options may appear before or after arguments. Folders can be referred to by **path**, by **name**
(case-insensitive, if unique) or by the **start of their ID** (at least 4 characters, see `veilfold list`).

## Options for every command

| Option | Description |
| --- | --- |
| `--data-dir DIR` | Use `DIR` as the data directory instead of the default (also `VEILFOLD_DATA_DIR`). |
| `--password-stdin` | Read passwords (and other prompts) from standard input, one per line, instead of the terminal. |
| `-h`, `--help` | Show help. |

## Vault commands

### `veilfold init`

Create a new vault. Prompts for a password twice (at least 8 characters) and prints a recovery key.

| Option | |
| --- | --- |
| `--no-recovery-key` | Do not create a recovery key. |

A computer can hold several vaults; each password opens its own vault.

### `veilfold passwd`

Change the vault's password. Prompts for the current password, then the new one. The recovery key stays
valid.

### `veilfold recover`

Open a vault with its recovery key and set a new password.

### `veilfold recovery-key`

Create a new recovery key for the vault. The previous one stops working.

### `veilfold destroy`

Delete a vault. Only possible once it protects no folders. Asks you to type `destroy` unless `--yes` is
given.

## Folder commands

### `veilfold add <folder>...`

Protect folders. By default they are locked immediately.

| Option | |
| --- | --- |
| `--mode encrypt\|hide` | Protection mode (default `encrypt`). |
| `--no-lock` | Only register the folders; lock them later with `veilfold lock`. |

### `veilfold list`

List protected folders with their state, mode and size.

| Option | |
| --- | --- |
| `--json` | Print the list as JSON (fields: `id`, `name`, `path`, `mode`, `locked`, `storage`, `size`, `files`, `added_at`, `locked_at`, `busy`, `problem`). |

In the `MODE` column, `hide*` marks a folder that should have been encrypted but is currently stored
hidden because an interrupted lock fell back to hiding it (see the architecture document). Unlock and
lock it again to encrypt it.

### `veilfold lock <folder>... | --all`

Lock folders. Locking a locked folder does nothing.

### `veilfold unlock <folder>... | --all`

Unlock folders to where they were.

| Option | |
| --- | --- |
| `--to PATH` | Restore the folder to `PATH` instead (one folder only). The folder is then protected at its new location. |

If the original location is taken (something new was created there), unlocking fails and suggests
`--to`.

### `veilfold remove <folder>...`

Stop protecting folders. Locked folders are unlocked first; nothing is deleted.

| Option | |
| --- | --- |
| `--forget` | Drop locked folders whose data can no longer be found (for example on an erased drive). Data that still exists is never discarded. |

### `veilfold mode <folder> encrypt|hide`

Change how an unlocked folder will be protected the next time it is locked.

## Other commands

| Command | |
| --- | --- |
| `veilfold info` | Print the version, data directory, default store and number of vaults. No password needed. |
| `veilfold version` | Print the version. |
| `veilfold help [command]` | Show help. |

## Progress and cancelling

When standard error is a terminal, long operations show a progress line. Press **Ctrl+C** to cancel: an
interrupted lock is rolled back and the folder is put back in place; an interrupted unlock leaves the
folder locked.

## Exit status

| Code | Meaning |
| --- | --- |
| 0 | Success. |
| 1 | An operation failed (details on standard error). |
| 2 | The command line was invalid. |

## Environment

| Variable | |
| --- | --- |
| `VEILFOLD_DATA_DIR` | Default for `--data-dir`. |

## Scripting example

```sh
# Lock everything at the end of a session (password from a secrets manager).
secret-tool lookup app veilfold | veilfold --password-stdin lock --all

# Machine-readable status.
secret-tool lookup app veilfold | veilfold --password-stdin list --json | jq '.[] | {name, locked}'
```

With `--password-stdin`, commands that ask for several secrets read them in order, one per line:
`passwd` reads the current and then the new password; `recover` reads the recovery key and then the new
password; `destroy` reads the password and then the confirmation.

Passing passwords through environment variables or command lines exposes them to other processes of the
same user; prefer a secrets manager or the interactive prompt.
