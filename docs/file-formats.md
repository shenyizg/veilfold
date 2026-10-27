# File formats

This document specifies every file Veilfold writes, so that its data can be audited and, if ever
necessary, recovered by independent tools. All formats carry a version number; this document describes
version 1. Integers are big endian unless stated otherwise.

Notation: `‖` is concatenation, `HKDF(ikm, salt, info)` is HKDF-SHA256 producing 32 bytes,
`XSeal(key, plaintext, ad)` is XChaCha20-Poly1305 with a random 24-byte nonce producing
`nonce ‖ ciphertext ‖ tag`.

## Vault header — `vaults/<vault id>/vault.json`

Plain JSON. It contains nothing but the vault's master key wrapped by each key slot. Byte strings are
encoded as standard base64 (Go's encoding of `[]byte`).

```json
{
  "format": "veilfold-vault",
  "version": 1,
  "id": "3f2a9c1b6d0e4f7a8b9c0d1e2f3a4b5c",
  "password": {
    "kdf": "argon2id",
    "params": { "memory_kib": 65536, "iterations": 3, "parallelism": 4 },
    "salt": "<16 bytes>",
    "wrapped_key": "<72 bytes>"
  },
  "recovery": {
    "kdf": "hkdf-sha256",
    "salt": "<32 bytes>",
    "wrapped_key": "<72 bytes>"
  }
}
```

- `id` is 16 random bytes in lowercase hex and equals the directory name.
- Password slot: `KEK = Argon2id(password, salt, iterations, memory_kib, parallelism, 32)`,
  `wrapped_key = XSeal(KEK, master_key, "veilfold vault key v1" ‖ 0x00 ‖ id ‖ 0x00 ‖ "password")`.
- Recovery slot (optional): `KEK = HKDF(recovery_key, salt, "veilfold/recovery/v1")`,
  `wrapped_key = XSeal(KEK, master_key, "veilfold vault key v1" ‖ 0x00 ‖ id ‖ 0x00 ‖ "recovery")`.
- Readers must reject Argon2id parameters outside: parallelism 1–64, iterations 1–64,
  memory 8·parallelism KiB to 4 GiB.

### Recovery key text

`text = CrockfordBase32(key ‖ SHA-256("veilfold recovery key v1" ‖ key)[0:4])`, where `key` is 16
random bytes, giving 32 characters (alphabet `0123456789ABCDEFGHJKMNPQRSTVWXYZ`, no padding), displayed
in eight groups of four separated by `-`. When parsing, case, spaces and dashes are ignored, and `I`/`L`
are read as `1`, `O` as `0`.

## Index — `vaults/<vault id>/index.vfi`

```text
"VFI1" ‖ XSeal(index_key, json, "veilfold index v1" ‖ 0x00 ‖ vault_id)
index_key = HKDF(master_key, salt = empty, info = "veilfold/index/v1")
```

`index.vfi.bak` is a copy in the same format, written before `index.vfi` on every save. The JSON
document:

```json
{
  "version": 1,
  "items": [
    {
      "id": "a1b2…",                  // 16 random bytes, hex
      "path": "/Users/alice/Documents/Taxes",
      "mode": "encrypt",              // or "hide": protection applied on the next lock
      "locked": true,
      "storage": "sealed",            // when locked: "sealed" (archive) or "hidden" (folder)
      "store_dir": "/Users/alice/Library/Application Support/Veilfold/store.noindex",
      "conceal": 0,                   // hidden folders: Unix permission bits / Windows attributes to restore
      "concealed": false,             // hidden folders: whether concealment was applied
      "blob_salt": "9f…",             // sealed folders: hex salt of the archive made by the last lock
      "size": 1288490188,             // content bytes at the last lock (sealed)
      "files": 412,
      "added_at": "2026-10-01T09:30:00Z",
      "locked_at": "2026-10-27T08:12:44Z",
      "pending": null                 // in-flight operation, see architecture.md
    }
  ]
}
```

A locked item's data is at `store_dir/<id>.veil` (sealed) or `store_dir/<id>` (hidden). When unlocking
a sealed item, the salt in the archive header must equal `blob_salt`, so an older archive of the same
folder cannot be substituted.

`pending`, when present, is `{phase, store_dir, stage, target, conceal, root, size, files}`, where `phase`
is one of `hide/move`, `unhide/move`, `seal/stage`, `seal/write`, `seal/abort`, `seal/cleanup`,
`unseal/extract`, `unseal/rename`, `unseal/cleanup`, and `root` holds the metadata of a folder being restored (see
`DirMeta` below).

## Encrypted folder — `<store>/<item id>.veil`

```text
offset  size  field
0       4     magic "VEIL"
4       1     version = 1
5       3     reserved, zero
8       16    item id (raw bytes)
24      32    salt (random per archive)
56      …     STREAM ciphertext
```

- `archive_key = HKDF(master_key, salt, "veilfold/blob/v1" ‖ 0x00 ‖ hex(item id))`.
- The plaintext (a pack stream, below) is split into chunks of 65536 bytes; only the last may be
  shorter, and it is empty only if the whole plaintext is empty.
- Chunk `i` is `ChaCha20-Poly1305(archive_key, nonce_i, chunk_i, ad = header[0:56])` (ciphertext ‖ 16-byte
  tag), where `nonce_i = 0x000000 ‖ uint64(i) ‖ flag` (12 bytes) and `flag` is `0x01` for the last chunk and
  `0x00` otherwise.
- A reader treats a chunk as the last one if fewer than 65552 bytes remain or nothing follows it, and
  must fail if authentication fails, if data follows the last chunk, or if an empty last chunk follows
  other chunks.

## Pack stream (plaintext of an encrypted folder)

```text
"VFPK" ‖ version u8 (= 1) ‖ meta_len u32 ‖ meta (JSON: {"item", "source", "created"})
entry*
'E' ‖ dirs u64 ‖ files u64 ‖ symlinks u64 ‖ bytes u64
```

Each entry:

```text
kind      u8      'D' directory, 'F' regular file, 'L' symbolic link
path      u32 + bytes   slash-separated, relative to the root; "." is the root itself
mode      u32     Unix permission bits incl. setuid (0o4000), setgid (0o2000), sticky (0o1000)
mtime     i64     modification time, nanoseconds since the Unix epoch
attrs     u32     Windows file attributes; 0 on other systems
xattrs    u16 count, then per attribute: name (u16 + bytes), value (u32 + bytes)
'F' only: size u64, followed by exactly `size` bytes of content
'L' only: target (u32 + bytes)
```

Rules (enforced by the reader):

- The first entry is the root directory (`kind = 'D'`, `path = "."`) and appears once.
- Entries are in pre-order: a directory precedes its contents.
- Paths must be clean (`path.Clean(p) == p`), relative, free of `..` and NUL, and local to the destination
  on the current platform. Symbolic links are created only after every file and directory, so an archive
  cannot redirect writes through a link. Symbolic links are never produced on Windows.
- The trailer must match the counted entries and total content bytes, and nothing may follow it.
- Limits: metadata ≤ 1 MiB, paths and link targets ≤ 64 KiB, extended attribute names ≤ 4 KiB and
  values ≤ 64 MiB.

Extended attributes are recorded on macOS (all) and Linux (`user.*` only), and restored best-effort.
Directory metadata is applied bottom-up after extraction; the root's metadata (`DirMeta`:
`{mode, mtime, attrs, xattrs}`) is returned to the caller, which applies it last.

## Desktop app settings — `gui-settings.json`

```json
{ "defaultMode": "encrypt", "autoLockMinutes": 0 }
```

Not secret, not encrypted.
