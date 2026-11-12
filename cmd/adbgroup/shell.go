package main

import (
	"errors"
	"fmt"
	"io"
)

func runShellInit(args []string, out io.Writer) error {
	flags := newCommandFlags("shell-init")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 || flags.Arg(0) != "zsh" && flags.Arg(0) != "bash" {
		return errors.New("usage: adg shell-init <zsh|bash>")
	}
	cleanup := bashSwitch
	if flags.Arg(0) == "zsh" {
		cleanup = zshSwitch
	}
	_, err := fmt.Fprint(out, shellApply+cleanup)
	return err
}

const shellApply = `# adbgroup shell integration; safe to load repeatedly.
_adbgroup_apply() {
    local _ag_file="$1" _ag_action _ag_socket _ag_context _ag_status
    shift
    if ADBGROUP_SHELL_STATE_FILE="$_ag_file" command adg "$@"; then
        _ag_status=0
    else
        _ag_status=$?
        return "$_ag_status"
    fi
    # Help succeeds without producing a state change.
    [ -s "$_ag_file" ] || return 0
    {
        IFS= read -r _ag_action &&
        IFS= read -r _ag_socket &&
        IFS= read -r _ag_context
    } < "$_ag_file" || return 1
    case "$_ag_action" in
        set)
            [ -n "$_ag_socket" ] && [ -n "$_ag_context" ] || return 1
            if [ "${_ADBGROUP_HAS_SAVED-}" != 1 ]; then
                _ADBGROUP_SAVED_SOCKET_SET="${ADB_SERVER_SOCKET+x}"
                _ADBGROUP_SAVED_SOCKET="${ADB_SERVER_SOCKET-}"
                _ADBGROUP_HAS_SAVED=1
            fi
            export ADB_SERVER_SOCKET="$_ag_socket"
            export ADBGROUP_CONTEXT="$_ag_context"
            ;;
        unset)
            if [ "${_ADBGROUP_HAS_SAVED-}" = 1 ]; then
                if [ "$_ADBGROUP_SAVED_SOCKET_SET" = x ]; then
                    export ADB_SERVER_SOCKET="$_ADBGROUP_SAVED_SOCKET"
                else
                    unset ADB_SERVER_SOCKET
                fi
            fi
            unset ADBGROUP_CONTEXT _ADBGROUP_HAS_SAVED _ADBGROUP_SAVED_SOCKET_SET _ADBGROUP_SAVED_SOCKET
            ;;
        *) return 1 ;;
    esac
}
`

const zshSwitch = `adg() {
    case "${1-}" in
        use|unuse) ;;
        *) command adg "$@"; return $? ;;
    esac
    local _ag_temp
    _ag_temp=$(command mktemp "${TMPDIR:-/tmp}/adbgroup-shell.XXXXXXXX") || return
    {
        _adbgroup_apply "$_ag_temp" "$@"
    } always {
        command rm -f -- "$_ag_temp"
    }
}
`

const bashSwitch = `adg() {
    case "${1-}" in
        use|unuse) ;;
        *) command adg "$@"; return $? ;;
    esac
    local _ag_temp _ag_result=0 _ag_interrupted=0 _ag_old_int _ag_old_term
    _ag_temp=$(command mktemp "${TMPDIR:-/tmp}/adbgroup-shell.XXXXXXXX") || return
    _ag_old_int=$(trap -p INT)
    _ag_old_term=$(trap -p TERM)
    trap '_ag_interrupted=130; command rm -f -- "$_ag_temp"' INT
    trap '_ag_interrupted=143; command rm -f -- "$_ag_temp"' TERM
    if _adbgroup_apply "$_ag_temp" "$@"; then
        _ag_result=0
    else
        _ag_result=$?
    fi
    command rm -f -- "$_ag_temp"
    trap - INT TERM
    # These strings come from the shell's own trap builtin, never CLI output.
    eval "$_ag_old_int"
    eval "$_ag_old_term"
    [ "$_ag_interrupted" = 0 ] || _ag_result=$_ag_interrupted
    return "$_ag_result"
}
`
