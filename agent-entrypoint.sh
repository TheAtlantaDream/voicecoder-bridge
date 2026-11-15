#!/usr/bin/env bash
# Starts Claude Code behind AgentAPI in the project folder.
set -euo pipefail

project="${PROJECT_DIR:-$HOME/project}"

if [ -z "${ANTHROPIC_API_KEY:-}" ]; then
  echo "[agent] ANTHROPIC_API_KEY is not set, so no agent is started."
  exec sleep infinity
fi

# Haiku unless a model is chosen.
export ANTHROPIC_MODEL="${ANTHROPIC_MODEL:-claude-haiku-4-5}"

mkdir -p "$project"
[ -d "$project/.git" ] || git -C "$project" init -q
git config --global user.email >/dev/null || {
  git config --global user.name "VoiceCoder User"
  git config --global user.email "voicecoder@localhost"
}

# Claude Code opens with setup questions that nobody is there to answer.
# Mark them answered, keeping whatever else is already in the file.
config="$HOME/.claude.json"
[ -s "$config" ] || echo '{}' > "$config"
jq --arg dir "$project" --arg key "${ANTHROPIC_API_KEY: -20}" '
  .hasCompletedOnboarding = true
  | .hasAcknowledgedCostThreshold = true
  | .customApiKeyResponses.approved = ((.customApiKeyResponses.approved // []) + [$key] | unique)
  | .projects[$dir].hasTrustDialogAccepted = true
  | .projects[$dir].hasCompletedProjectOnboarding = true
' "$config" > "$config.tmp" && mv "$config.tmp" "$config"

# Accept the one-time warning shown before running without permission prompts.
settings="$HOME/.claude/settings.json"
mkdir -p "$HOME/.claude"
[ -s "$settings" ] || echo '{}' > "$settings"
jq '.skipDangerousModePermissionPrompt = true' "$settings" > "$settings.tmp" && mv "$settings.tmp" "$settings"

# Install the VoiceCoder rules as a marked section of Claude's own rules file.
# A section from an older version is replaced; the rest of the file (the
# user's own notes) is left alone.
rules_src="${VOICECODER_RULES:-/usr/local/share/voicecoder/agent-rules.md}"
rules_file="$HOME/.claude/CLAUDE.md"
if [ -s "$rules_src" ] && ! grep -qxF "$(head -n 1 "$rules_src")" "$rules_file" 2>/dev/null; then
  if [ -f "$rules_file" ]; then
    awk '
      /^# VoiceCoder rules/ { skip = 1; next }
      skip && /^# End VoiceCoder rules/ { skip = 0; next }
      skip && /^- / { next }
      skip && /^[[:space:]]*$/ { next }
      { skip = 0; print }
    ' "$rules_file" > "$rules_file.tmp" && mv "$rules_file.tmp" "$rules_file"
  fi
  if [ -s "$rules_file" ] && [ -n "$(tail -n 1 "$rules_file")" ]; then echo >> "$rules_file"; fi
  cat "$rules_src" >> "$rules_file"
  echo "[agent] wrote $(head -n 1 "$rules_src") to $rules_file"
fi

# What the agent learns about the user. It lives in the home volume on this
# machine and nowhere else.
profile="$HOME/.voicecoder/profile.md"
if [ ! -e "$profile" ]; then
  mkdir -p "$(dirname "$profile")"
  printf '# User profile\n\nNothing is known about the user yet.\n' > "$profile"
fi

cd "$project"
# The agent may edit files and run commands without asking. The user reviews
# the diff and approves by voice instead.
exec agentapi server --type=claude --allowed-hosts '*' -- claude --dangerously-skip-permissions
