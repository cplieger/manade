#!/usr/bin/env bash
# images/docker/manade-dockerd-run.sh, run whole as a profile's command runs
# it, against PATH stubs for sudo and docker. GitHub's run.sh is an absolute
# path this host lacks, so each run uses a copy whose one mention of it names
# a sentinel instead.
# SC2015: lib.sh's ok and no return 0, so `cond && ok || no` cannot mis-fire.
# shellcheck disable=SC2015
set -u

# shellcheck source-path=SCRIPTDIR
. "$(dirname -- "$0")/lib.sh"
ENTRYPOINT="${DOCKERD_RUN_SH:-$REPO_ROOT/images/docker/manade-dockerd-run.sh}"
new_workdir >/dev/null

load_function start_dockerd
runner=/home/runner/run.sh
n=$(grep -cF "$runner" "$ENTRYPOINT")
if [ "$n" -ne 1 ]; then
  no "the wrapper names GitHub's run.sh once" "found $n"
  report
  exit 1
fi
sed "s|$runner|$WORK/run.sh|" "$ENTRYPOINT" >"$WORK/wrapper.sh"

mkdir "$WORK/bin"
cat >"$WORK/bin/sudo" <<'EOF'
#!/usr/bin/env bash
printf '%s\0' "$@" >"$WORK/sudo.argv"
if [ "$STUB_SUDO" = refused ]; then
  printf 'sudo: refused by the stub\n' >&2
  exit 1
fi
if [ "$STUB_SUDO" = nesting ]; then
  printf 'manade-dockerd-run: nesting line from the stub\n' >&2
  printf 'level=info msg="dockerd noise from the stub"\n' >&2
fi
printf '%s' "$$" >"$WORK/sudo.pid"
exec sleep 30
EOF
# A daemon answers only after start_dockerd has run, as dockerd does.
cat >"$WORK/bin/docker" <<'EOF'
#!/usr/bin/env bash
[ "$*" = version ] || exit 2
[ "$STUB_DOCKER" = up ] && [ -s "$WORK/sudo.pid" ]
EOF
cat >"$WORK/run.sh" <<'EOF'
#!/usr/bin/env bash
printf 'run.sh %d\n' "$#" >"$WORK/ran"
EOF
cat >"$WORK/job.sh" <<'EOF'
#!/usr/bin/env bash
printf 'job.sh %s|%s\n' "$1" "$2" >"$WORK/ran"
EOF
chmod 755 "$WORK/bin/sudo" "$WORK/bin/docker" "$WORK/run.sh" "$WORK/job.sh"
export WORK

# wrap <sudo> <docker> [args...] runs the wrapper copy under a bound, with
# its exit status in $rc, and stops the stub daemon it left.
wrap() {
  rm -f "$WORK/ran" "$WORK/sudo.argv" "$WORK/sudo.pid" "$WORK/out" "$WORK/err"
  local sudo=$1 dockerd=$2
  shift 2
  rc=0
  STUB_SUDO=$sudo STUB_DOCKER=$dockerd TMPDIR=$WORK PATH="$WORK/bin:$PATH" \
    timeout 20 bash "$WORK/wrapper.sh" "$@" >"$WORK/out" 2>"$WORK/err" || rc=$?
  local i
  for i in $(seq 50); do
    [ -s "$WORK/sudo.argv" ] && break
    [ "$i" -lt 50 ] && sleep 0.1
  done
  if [ -s "$WORK/sudo.pid" ]; then
    kill "$(cat "$WORK/sudo.pid")" 2>/dev/null || :
  fi
}

wrap live up
[ "$rc" -eq 0 ] && ok "a daemon that answers runs the runner (exit 0)" || no "runner exit" "rc=$rc, stderr: $(cat "$WORK/err")"
[ "$(cat "$WORK/ran" 2>/dev/null)" = "run.sh 0" ] && ok "with no arguments the wrapper runs GitHub's run.sh with none" || no "zero-argument branch" "ran: $(cat "$WORK/ran" 2>/dev/null)"
argv=()
[ -s "$WORK/sudo.argv" ] && mapfile -d '' argv <"$WORK/sudo.argv"
want="$(declare -f start_dockerd); start_dockerd"
[ "${#argv[@]}" -eq 4 ] && [ "${argv[0]}" = -n ] && [ "${argv[1]}" = bash ] && [ "${argv[2]}" = -c ] && [ "${argv[3]}" = "$want" ] \
  && ok "the daemon starts through sudo -n as the shipped start_dockerd" \
  || no "sudo argv" "got ${#argv[@]} arguments: ${argv[*]:0:3}"

[ ! -s "$WORK/err" ] && ok "a daemon that starts cleanly adds nothing to stderr" || no "clean stderr" "stderr: $(cat "$WORK/err")"

case "$(declare -f start_dockerd)" in
  *"manade-dockerd-run: enabling cgroup nesting failed"*) ok "the shipped nesting failure line carries the wrapper's prefix" ;;
  *) no "nesting line prefix" "start_dockerd no longer prints it under manade-dockerd-run:" ;;
esac
wrap nesting up
err=$(cat "$WORK/err")
[ "$rc" -eq 0 ] && [ "$(cat "$WORK/ran" 2>/dev/null)" = "run.sh 0" ] \
  && ok "a daemon that answers after a nesting failure still runs the runner" \
  || no "nesting runner" "rc=$rc, ran: $(cat "$WORK/ran" 2>/dev/null)"
[ "$err" = "manade-dockerd-run: nesting line from the stub" ] \
  && ok "the wrapper's own log lines reach stderr before the runner, and dockerd's do not" \
  || no "nesting stderr" "stderr: $err"

wrap live up "$WORK/job.sh" one "two words"
[ "$rc" -eq 0 ] && [ "$(cat "$WORK/ran" 2>/dev/null)" = "job.sh one|two words" ] \
  && ok "with arguments the wrapper runs them in place of run.sh, word for word" \
  || no "argument branch" "rc=$rc, ran: $(cat "$WORK/ran" 2>/dev/null)"

wrap refused down
err=$(cat "$WORK/err")
[ "$rc" -eq 1 ] && ok "a refused sudo exits 1" || no "refused exit" "rc=$rc"
case "$err" in
  *"manade-dockerd-run: dockerd exited before it answered; its log:"*"sudo: refused by the stub"*)
    ok "a refused sudo is reported as an early exit with the daemon's log"
    ;;
  *) no "early-exit report" "stderr: $err" ;;
esac
[ ! -e "$WORK/ran" ] && ok "a refused sudo never runs the runner" || no "refused runner" "ran: $(cat "$WORK/ran")"

# SECONDS taken from the environment starts bash's clock there, so the
# 60-second bound is two seconds away
# (https://www.gnu.org/software/bash/manual/html_node/Bash-Variables.html#index-SECONDS).
SECONDS=58
export SECONDS
wrap live down
unset SECONDS
err=$(cat "$WORK/err")
[ "$rc" -eq 1 ] && ok "a daemon that never answers exits 1" || no "timeout exit" "rc=$rc"
case "$err" in
  *"manade-dockerd-run: dockerd did not answer within 60 seconds; its log:"*) ok "a silent daemon is reported as the 60-second bound" ;;
  *) no "timeout report" "stderr: $err" ;;
esac
[ ! -e "$WORK/ran" ] && ok "a silent daemon never runs the runner" || no "timeout runner" "ran: $(cat "$WORK/ran")"

report
