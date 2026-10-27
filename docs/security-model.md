# Security model

This document states what Veilfold protects, against whom, how, and — just as important — what it does
not do. If you believe Veilfold falls short of what is described here, please report it as described in
[SECURITY.md](../SECURITY.md).

## What is protected

1. **The contents** of protected folders: file data, file and folder names, and the directory structure.
2. **The identity** of protected folders: their names and where they were.
3. **Separation between people**: one person's vault reveals nothing about another person's vault on the
   same computer.

## Adversaries

| | Who | Example | Protected while locked? |
| --- | --- | --- | --- |
| A1 | Another person using the **same** operating-system account | family laptop with one login | **Yes**, both modes |
| A2 | Another **account** on the same computer | office PC with several users | **Yes**, both modes (plus OS permissions) |
| A3 | Someone with **administrator rights** or the **disk** in hand (offline) | IT staff, stolen laptop, repair shop | **Encrypt mode: yes.** Hide mode: no — hidden folders are concealed, not encrypted |
| A4 | **Malware**, a keylogger, a remote-access tool, or someone at the keyboard while the vault is open | — | **No** (out of scope) |

Veilfold is not a replacement for full-disk encryption (FileVault, BitLocker, LUKS) or for separate
operating-system accounts. It is designed to complement them.

## Guarantees

While a folder is **locked**:

- It does not exist at its original location. Nothing is left in the parent directory.
- In **encrypt** mode its contents, names and structure are encrypted with authenticated encryption under
  a key derived from the vault's master key. Without the password or recovery key they cannot be read,
  and any modification is detected on unlock.
- In **hide** mode the folder is moved, unchanged, into a store with all permissions removed (Unix) or with
  the hidden and system attributes set (Windows), under a random name, in a location that file managers
  and search indexes skip. The owner of the account or an administrator can still undo this and read the
  files; hide mode only defeats casual browsing and searching (A1, A2).
- The index of protected folders (names, locations, states) is encrypted in both modes.

While a folder is **unlocked** it is an ordinary folder with no protection at all.

## Cryptographic design

All primitives come from the Go standard library and `golang.org/x/crypto`.

### Key hierarchy

```text
password ──Argon2id(salt₁₆, 64 MiB, t=3, p=4)──► KEK ──XChaCha20-Poly1305──┐
recovery key (128 bit) ──HKDF-SHA256(salt₃₂)──► KEKᵣ ──XChaCha20-Poly1305──┤ unwrap
                                                                            ▼
                                                  master key (256 bit, random per vault)
                                                        │
                       ┌────────────────────────────────┴─────────────────────────────┐
         HKDF-SHA256(info="veilfold/index/v1")                HKDF-SHA256(salt=per-archive 256-bit salt,
                       │                                       info="veilfold/blob/v1"‖item id)
                       ▼                                                               ▼
     index key: XChaCha20-Poly1305                         archive key: ChaCha20-Poly1305 STREAM
```

- **Password slot.** Argon2id with 64 MiB of memory, 3 passes and 4 lanes (the parameters are stored per
  vault and can be raised later; changing the password re-wraps with the current defaults). The wrapped
  master key is bound to the vault ID and slot name as associated data.
- **Recovery slot.** A 128-bit random key shown once to the user (32 characters of Crockford base32
  including a 32-bit checksum that catches typos). Because it has full entropy, HKDF suffices.
- **Changing the password** only re-wraps the master key; folders are not re-encrypted.
- **Index.** XChaCha20-Poly1305 with a fresh random 192-bit nonce on every save; the vault ID is
  authenticated as associated data.
- **Folder archives.** Each lock creates a new archive with a fresh 256-bit salt and therefore a fresh key.
  The archive is encrypted with ChaCha20-Poly1305 in the STREAM construction (Hoang, Reyhanitabar,
  Rogaway and Vizár, 2015), like [age](https://age-encryption.org/v1): 64 KiB chunks, nonce = 88-bit
  chunk counter ‖ last-chunk flag, the 56-byte archive header as associated data of every chunk. This
  detects truncation, reordering, duplication and any bit flip. Data is only released after its chunk is
  authenticated.
- **Verification.** After writing an archive, Veilfold decrypts and parses all of it before the original
  folder is deleted.

### Multiple vaults

A vault is selected by trying the password against every vault header. The cost of opening therefore
grows with the number of vaults (about 0.1–0.5 s of Argon2id each, run in parallel). Two vaults in the
same data directory cannot share a password — Veilfold refuses, because it could not tell them apart.
Note that a person who knows another vault's password can of course open it.

## What an attacker can learn

Even without any password, someone with access to the data directory and stores can see:

- that Veilfold is installed, and how many vaults exist;
- how many items are locked in each store, and when they were locked (file timestamps);
- the approximate total size of each encrypted folder (archive size ≈ content size + 0.03 %);
- for **hidden** folders: everything inside them, if they have the privileges to restore permissions.

They cannot learn folder names, original locations, or which vault an item belongs to.

## Limitations and recommendations

- **Remnants of deleted plaintext.** After encrypting, Veilfold deletes the original files normally. On
  hard disks the data stays recoverable until overwritten; on SSDs (wear levelling, TRIM) and on
  copy-on-write or journaling file systems (APFS, Btrfs, ZFS, NTFS journal) old blocks may persist.
  Veilfold does not attempt "secure deletion" because it cannot be done reliably on modern storage.
  **Use full-disk encryption** to make remnants useless to an offline attacker.
- **Other copies of your files.** Backups (Time Machine, File History), cloud sync, search indexes
  built before the folder was protected, thumbnail caches, "recent files" lists, swap and hibernation
  files can contain copies or traces. Exclude sensitive folders from them or encrypt them as well.
- **Keys in memory.** While a vault is open its master key is in the process's memory. Go cannot
  guarantee that every copy of a secret is wiped, though Veilfold wipes the buffers it controls. Close
  the vault (or enable auto-lock) when you are done.
- **Password strength.** An offline attacker can try passwords against `vault.json` at the speed of
  Argon2id. Veilfold requires at least 8 characters; use a long passphrase.
- **Recovery key handling.** Anyone holding the recovery key can reset the vault's password. Store it
  like a password; replace it (`veilfold recovery-key`, or Settings in the app) if it may have been seen.
- **No protection against the index being rolled back.** An attacker who can write to the data directory
  can replace the index with an older version. This cannot reveal or forge data but can make Veilfold
  show an outdated state. (The index does bind each locked folder to its current archive, so an older
  archive cannot be swapped in while the index stays current.)
- **What encrypt mode does not carry over.** File contents, names, directory structure, permission bits,
  modification times and extended attributes (macOS; `user.*` on Linux) are preserved. Not preserved:
  ACLs, BSD file flags such as Finder's "Locked", Windows alternate data streams, file creation times,
  and hard links (linked files come back as separate copies). Use hide mode if any of these matter.
- **Concealment depends on the file system.** On FAT and exFAT drives, permissions and hidden flags do not
  exist, so a hidden folder is concealed only by its location and random name. Prefer encrypt mode on such
  drives.
- **Shared drives.** On drives used by several accounts, Veilfold only stores data in a directory that is
  owned by the current user, is not a link and is private (`0700`); otherwise it refuses to use that
  location.
- **Availability.** Anyone with write access to the data directory or a store can delete locked data.
  Veilfold protects confidentiality, not against destruction — keep backups.
- **Unsigned builds.** Until release builds are code-signed, verify downloads against `SHA256SUMS.txt`
  from the release page, or build from source.

## Reporting

Please report vulnerabilities privately; see [SECURITY.md](../SECURITY.md).
