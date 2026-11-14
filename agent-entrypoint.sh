#!/usr/bin/env bash
# Starts Claude Code behind AgentAPI in the project folder.
set -euo pipefail

project="${PROJECT_DIR:-$HOME/project}"

if [ -z "${ANTHROPIC_API_KEY:-}" ]; then
  echo "[agent] ANTHROPIC_API_KEY is not set, so no agent is started."
  exec sleep infinity
fi

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

cd "$project"
# The agent may edit files and run commands without asking. The user reviews
# the diff and approves by voice instead.
exec agentapi server --type=claude --allowed-hosts '*' -- claude --dangerously-skip-permissions
