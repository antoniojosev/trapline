#!/usr/bin/env bash
#
# One rule, shared by every gate that starts a server: check the port is free
# before starting, and abort naming it if it is not.
#
# Without this a script connects to whatever is already listening — a leftover
# from another branch, an unrelated project that claimed the same number — and
# then fails much later with a symptom that looks nothing like the cause. The
# real case: `stats.sh` talked to a residual server from another branch that
# had a different admin password, and reported `invalid credentials` from the
# statistics gate. Nothing in that message points at a port.
#
# Sourced rather than duplicated, because a rule that lives in five copies is
# a rule that is four edits away from being wrong in one of them.

# require_free_port PORT [WHAT]
#
# Aborts with a message that names the port, what wanted it, and what to do
# about it.
require_free_port() {
  local port="$1"
  local what="${2:-this gate}"

  # bash's own TCP redirection, so this needs neither ss, lsof nor nc — none
  # of which is present in every container this runs in. A connection that
  # succeeds means something is listening; anything else means nothing is.
  if (exec 3<>"/dev/tcp/127.0.0.1/$port") 2>/dev/null; then
    exec 3<&- 3>&-
    printf '\n   FAIL port %s is already in use, so %s did not start.\n' "$port" "$what" >&2
    printf '        It would otherwise have talked to whatever is listening there — a\n' >&2
    printf '        leftover server from another branch, or an unrelated project — and\n' >&2
    printf '        failed later with a symptom that looks nothing like this cause.\n\n' >&2
    printf '        Find it with:  ss -ltnp | grep :%s\n' "$port" >&2
    printf '        Or run this gate elsewhere:  PORT=<free port> %s\n\n' "$0" >&2
    exit 1
  fi
}
