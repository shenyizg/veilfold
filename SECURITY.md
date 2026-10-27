# Security Policy

Veilfold exists to keep people's private files private, so we take security reports seriously and
appreciate the work that goes into them.

## Supported versions

Veilfold is young. Security fixes are made for the **latest released version** only; please upgrade
before reporting.

| Version | Supported |
| ------- | --------- |
| 0.1.x   | ✅        |
| < 0.1   | ❌        |

## Reporting a vulnerability

**Please do not open a public issue, discussion or pull request for security problems.**

Report privately through GitHub's private vulnerability reporting:
<https://github.com/shenyizg/veilfold/security/advisories/new>
(or the repository's **Security** tab → **Report a vulnerability**).

Please include:

- the affected version and operating system;
- a description of the issue and its impact;
- steps to reproduce, a proof of concept, or the relevant code location;
- whether the issue is already public or known to others.

### What to expect

- **Acknowledgement** within 3 working days.
- **Assessment** (confirmed or not, severity) within 10 working days.
- A **fix** as fast as the severity requires, normally within 90 days, released together with a
  security advisory that credits you unless you prefer to stay anonymous.

We follow coordinated disclosure: please give us a reasonable chance to release a fix before publishing
details.

## Scope

The threat model is described in [docs/security-model.md](docs/security-model.md). In scope, for example:

- weaknesses in the cryptography or its use (key derivation, encryption, authentication, randomness);
- reading the contents, names or locations of locked, **encrypted** folders without the password or
  recovery key;
- bugs that lose or corrupt user data, or leave unencrypted copies behind without telling the user;
- bypasses of path validation that let Veilfold move system folders or other users' data;
- one vault revealing information about another vault;
- vulnerabilities in the desktop app's bridge between the web view and the backend.

Out of scope (documented limitations):

- attacks by malware, keyloggers or anyone with access to the computer while the vault is open;
- reading **hidden** (not encrypted) folders with administrator rights or physical access;
- recovering deleted plaintext from SSDs or journaling file systems;
- deleting locked data (Veilfold protects confidentiality, not availability);
- issues that require social engineering or a modified Veilfold binary.

## Safe harbor

We will not take legal action against people who research and report vulnerabilities in good faith,
avoid harming users and their data, and respect the disclosure process above.
