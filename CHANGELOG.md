# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project
adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Fixed

- Desktop app: buttons and choices inside dialogs did not respond. **Protect**, **Change password** and
  **Create recovery key** did nothing, and the Encrypt/Hide choice and the "Lock the folder now" box
  could not be changed.
- Desktop app on macOS: **Lock all**, **Settings** and **Close vault** did not respond on recent macOS
  versions, where the inset title bar's toolbar area took the clicks. The app now uses the standard
  title bar.
- Desktop app: the "Lock the folder now" choice is kept when the dialog updates with the folder's size,
  and selecting text in a dialog's field no longer closes the dialog when the mouse is released outside.

## [0.1.0] - 2026-10-27

First public release.

### Added

- Vaults protected by a password (Argon2id) with an optional recovery key. Several independent vaults
  can share one computer account; the password selects the vault.
- **Encrypt mode**: a folder is encrypted (ChaCha20-Poly1305 STREAM) into a single archive and removed
  from its location. The archive is verified by decrypting it before the original is deleted.
- **Hide mode**: a folder is moved into a concealed store on the same drive, instantly and unchanged.
- Encrypted index of protected folders, so their names and locations are not visible without the
  password.
- Per-drive stores, so folders on external drives are never copied across drives.
- Journaled, crash-safe locking and unlocking with automatic repair the next time a vault is opened;
  folders whose repair cannot finish are protected from further changes and can be restored elsewhere.
- Before an archive replaces a folder, the folder is re-scanned so that changes made by programs that
  still had files open are never lost.
- Folders containing other mounted drives or network shares are refused in encrypt mode, and
  deleting temporary copies never crosses into mounted file systems.
- Reports of leftover data that the index does not account for when a vault is opened.
- Protection rules that refuse system folders, the home folder, drive roots and overlapping folders,
  and warnings for cloud-synced and very large folders.
- `veilfold` command-line tool for macOS, Windows and Linux.
- Desktop app for macOS and Windows: drag and drop, progress with cancel, unlocking to another
  location, auto-lock after inactivity, password and recovery key management, light and dark themes.

[Unreleased]: https://github.com/shenyizg/veilfold/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/shenyizg/veilfold/releases/tag/v0.1.0
