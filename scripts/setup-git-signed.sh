#!/usr/bin/env bash
# scripts/setup-git-signed.sh
#
# Apply the repo-local git config required by RAN-53 acceptance #6 (signed
# commits on main). Supports BOTH ssh-format and openpgp-format signing —
# picks up whichever the contributor already has wired into their global git
# config.
#
# Defaults (when nothing is set globally):
#   user.signingkey  = ~/.ssh/id_ed25519.pub
#   gpg.format       = ssh
#
# Honored env / global-config inputs:
#   GIT_USER_NAME       (else: git config --global user.name)
#   GIT_USER_EMAIL      (else: git config --global user.email)
#   GIT_SIGNING_KEY     (else: git config --global user.signingkey,
#                              else default SSH key)
#   GIT_GPG_FORMAT      (else: git config --global gpg.format,
#                              else "ssh")
#
# Idempotent: re-running is a no-op except for the verification block at the end.
# Run from the repo root (or any subdirectory of the worktree).

set -euo pipefail

# Resolve the worktree root and refuse to run anywhere else.
if ! repo_root=$(git rev-parse --show-toplevel 2>/dev/null); then
  echo "error: not inside a git working tree." >&2
  exit 1
fi

cd "$repo_root"

# Identity is taken from env vars first, then from the user's GLOBAL git
# config — never hard-coded to the maintainer. This avoids silently
# misattributing every contributor's signed commits to the maintainer.
GIT_USER_NAME=${GIT_USER_NAME:-$(git config --global --get user.name 2>/dev/null || true)}
GIT_USER_EMAIL=${GIT_USER_EMAIL:-$(git config --global --get user.email 2>/dev/null || true)}
GIT_SIGNING_KEY=${GIT_SIGNING_KEY:-$(git config --global --get user.signingkey 2>/dev/null || echo "$HOME/.ssh/id_ed25519.pub")}
GIT_GPG_FORMAT=${GIT_GPG_FORMAT:-$(git config --global --get gpg.format 2>/dev/null || echo "ssh")}

if [ -z "$GIT_USER_NAME" ] || [ -z "$GIT_USER_EMAIL" ]; then
  cat >&2 <<'EOF'
error: contributor identity not set.

This script does not assume a default identity. Set yours either:
  1. Globally (recommended):
       git config --global user.name  "Your Name"
       git config --global user.email "you@example.com"
  2. Per-invocation:
       GIT_USER_NAME="Your Name" GIT_USER_EMAIL="you@example.com" \
         scripts/setup-git-signed.sh

Then re-run this script. Signed commits will use the identity you set.
EOF
  exit 4
fi

# Signing-key validation depends on gpg.format:
#   - ssh:     user.signingkey is a path on disk (the file must exist)
#   - openpgp: user.signingkey is a key id / fingerprint (gpg must know it)
#   - x509:    user.signingkey is a key id / fingerprint (gpgsm must know it)
case "$GIT_GPG_FORMAT" in
  ssh)
    if [ ! -f "$GIT_SIGNING_KEY" ]; then
      cat >&2 <<EOF
error: SSH signing key not found at $GIT_SIGNING_KEY

Generate one with:
    ssh-keygen -t ed25519 -C "$GIT_USER_EMAIL"
Then upload the public key (\$GIT_SIGNING_KEY) to your GitHub account under:
    Settings → SSH and GPG keys → New SSH key → Key type: Signing Key
And re-run this script.
EOF
      exit 2
    fi
    ;;
  openpgp|gpg)
    if ! gpg --list-secret-keys --with-colons "$GIT_SIGNING_KEY" 2>/dev/null | grep -q '^sec:'; then
      cat >&2 <<EOF
error: OpenPGP signing key '$GIT_SIGNING_KEY' not found in your gpg keyring.

Either point user.signingkey at a key id / fingerprint that
\`gpg --list-secret-keys\` knows, or generate / import a key first:
    gpg --full-generate-key
    git config --global user.signingkey <KEY_ID>
And re-run this script.
EOF
      exit 2
    fi
    ;;
  x509)
    if ! gpgsm --list-secret-keys "$GIT_SIGNING_KEY" >/dev/null 2>&1; then
      cat >&2 <<EOF
error: x509 signing key '$GIT_SIGNING_KEY' not found via gpgsm.

Configure x509 signing keys in gpgsm and ensure
\`gpgsm --list-secret-keys\` reports your key, then re-run.
EOF
      exit 2
    fi
    ;;
  *)
    echo "error: unsupported gpg.format '$GIT_GPG_FORMAT' (expected ssh, openpgp, gpg, or x509)." >&2
    exit 5
    ;;
esac

apply() {
  local key="$1" value="$2"
  git config --local "$key" "$value"
}

apply user.name        "$GIT_USER_NAME"
apply user.email       "$GIT_USER_EMAIL"
apply user.signingkey  "$GIT_SIGNING_KEY"
apply gpg.format       "$GIT_GPG_FORMAT"
apply commit.gpgsign   true
apply tag.gpgsign      true

echo "Applied repo-local git config:"
printf "  %-22s = %s\n" \
  user.name        "$(git config --local --get user.name)" \
  user.email       "$(git config --local --get user.email)" \
  user.signingkey  "$(git config --local --get user.signingkey)" \
  gpg.format       "$(git config --local --get gpg.format)" \
  commit.gpgsign   "$(git config --local --get commit.gpgsign)" \
  tag.gpgsign      "$(git config --local --get tag.gpgsign)"

# Verification: produce a throwaway signed object and verify it.
# `git commit-tree` does not touch refs, so this is non-destructive.
echo
echo "Verifying signing produces a valid signature ..."
tree=$(git write-tree)
sig_commit=$(echo "setup-git-signed.sh verification" | git commit-tree "$tree" -S)
if git verify-commit --raw "$sig_commit" 2>&1 | grep -q '^GOODSIG\|^SSH_OK\|GOOD signature'; then
  echo "  ok — signing chain is healthy."
elif git verify-commit "$sig_commit" >/dev/null 2>&1; then
  echo "  ok — signing chain is healthy."
else
  cat >&2 <<EOF
warning: signing config applied but verification failed.

  Most common cause: the corresponding allowed-signers file is missing.
  Configure one with:
    git config --local gpg.ssh.allowedSignersFile ~/.config/git/allowed_signers
    printf "$GIT_USER_EMAIL %s\n" "\$(cat $GIT_SIGNING_KEY)" \\
      >> ~/.config/git/allowed_signers
  Then re-run this script.
EOF
  exit 3
fi

echo
echo "done. Every commit and tag from this worktree will now be ssh-signed."
