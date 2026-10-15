# One readable line per event of a Claude Code turn's stream, for the
# recorded Mac-to-Pi teleport demo (script/demo/teleport-mac-to-pi.tape).
# The stream is `claude -p --output-format stream-json --verbose`: one JSON
# object a line. Run it as
#
#   jq -R -r --unbuffered -f claude-feed.jq
#
# -R takes each line as text, so a line that is not JSON (a notice from
# brig) is dropped rather than ending jq. Tools are shown as Claude Code
# shows them, Read(file) with the file's base name; the text Claude writes
# is shown as it wrote it, indented; the result line is `✓ done: ...`.
# Two system events that say nothing about the turn (commands_changed,
# status) are left out; the other system events are shown by name.

def esc: "\u001b[";
def dim: esc + "2m" + . + esc + "0m";
def green: esc + "32m" + . + esc + "0m";
def cyan: esc + "36m" + . + esc + "0m";
def red: esc + "31m" + . + esc + "0m";
def base: tostring | sub(".*/"; "");
def short($n): tostring | gsub("\\s+"; " ") | if length > $n then .[0:$n] + "…" else . end;

# What a tool was called with, as one short argument.
def arg:
  if .name == "Read" or .name == "Write" or .name == "Edit" then (.input.file_path // "" | base)
  elif .name == "Bash" then (.input.command // "" | short(40))
  elif .name == "Glob" or .name == "Grep" then (.input.pattern // "" | short(40))
  else (.input | tostring | short(40))
  end;

fromjson? // empty
| if .type == "system" and .subtype == "init" then
    "○ claude -p in \(.cwd // "?"), model \(.model // "?")" | dim
  elif .type == "system" and (.subtype == "commands_changed" or .subtype == "status") then
    empty
  elif .type == "system" then
    "· \(.subtype // "system" | gsub("_"; " "))\(if .error then ": " + (.error | short(50)) else "" end)" | dim
  elif .type == "assistant" then
    (.message.content // [])[]
    | if .type == "tool_use" then "● " + ("\(.name)" | cyan) + "(\(arg))"
      elif .type == "text" and (.text | test("\\S")) then
        .text | sub("\\s+$"; "") | split("\n") | map("  " + .) | join("\n")
        | if test("^  -") then . else "● " + .[2:] end
      else empty
      end
  elif .type == "user" then
    (.message.content // [])[]
    | select(type == "object" and .type == "tool_result")
    | if .is_error then "  └ " + ((.content | tostring | short(50)) | red)
      else "  └ " + ("\(.content | if type == "array" then map(.text? // "") | join("") else tostring end | split("\n") | length) lines" | dim)
      end
  elif .type == "result" then
    if .is_error or .subtype != "success" then
      "✗ " + ("\(if .subtype == "success" then "error" else .subtype end): \(.result // "" | short(60))" | red)
    else
      ("✓ done: \(.num_turns) turns in \((.duration_ms // 0) / 1000 | floor) s" | green)
    end
  else empty
  end
