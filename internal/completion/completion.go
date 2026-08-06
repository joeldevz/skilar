package completion

// Bash returns a bash completion script for skynex.
func Bash() string {
	return `# skynex bash completion
_skynex_completions() {
    local cur prev commands profile_commands
    COMPREPLY=()
    cur="${COMP_WORDS[COMP_CWORD]}"
    prev="${COMP_WORDS[COMP_CWORD-1]}"

    commands="install doctor version profile up completion status update"
    profile_commands="list create"

    case "${COMP_CWORD}" in
        1)
            COMPREPLY=($(compgen -W "${commands}" -- "${cur}"))
            return 0
            ;;
        2)
            case "${prev}" in
                profile)
                    # list, create, or profile names
                    local profiles
                    profiles=$(skynex profile list 2>/dev/null | grep -E '^\s+\S+' | awk '{print $1}' | grep -v '^Built-in$\|^Custom$\|^No$\|^Create$\|^Default:\|^Usage:')
                    COMPREPLY=($(compgen -W "${profile_commands} cheap balanced premium ${profiles}" -- "${cur}"))
                    return 0
                    ;;
                up)
                    local profiles
                    profiles=$(skynex profile list 2>/dev/null | grep -E '^\s+\S+' | awk '{print $1}' | grep -v '^Built-in$\|^Custom$\|^No$\|^Create$\|^Default:\|^Usage:')
                    COMPREPLY=($(compgen -W "cheap balanced premium ${profiles} --web --port" -- "${cur}"))
                    return 0
                    ;;
                completion)
                    COMPREPLY=($(compgen -W "bash zsh fish" -- "${cur}"))
                    return 0
                    ;;
                install)
                    COMPREPLY=($(compgen -W "--package --target --version --non-interactive --yes --verbose --trust-setup-scripts --state-dir" -- "${cur}"))
                    return 0
                    ;;
            esac
            ;;
        3)
            # After profile <name>, suggest verbs
            if [[ "${COMP_WORDS[1]}" == "profile" ]]; then
                case "${prev}" in
                    list|create) return 0 ;;
                    *) COMPREPLY=($(compgen -W "edit delete default" -- "${cur}")) ; return 0 ;;
                esac
            fi
            if [[ "${COMP_WORDS[1]}" == "up" ]]; then
                COMPREPLY=($(compgen -W "--web --port" -- "${cur}"))
                return 0
            fi
            ;;
    esac
}
complete -F _skynex_completions skynex
`
}

// Zsh returns a zsh completion script for skynex.
func Zsh() string {
	return `#compdef skynex
# skynex zsh completion

_skynex() {
    local -a commands profile_subcommands shells tiers

    commands=(
        'install:Interactive installer (TUI)'
        'doctor:Check environment and dependencies'
        'version:Show version'
        'profile:Manage profiles (list, create, edit, delete)'
        'up:Launch OpenCode with a profile'
        'completion:Generate shell completion script'
        'status:Show environment status dashboard'
        'update:Update installed packages'
    )

    profile_subcommands=(
        'list:List all profiles (builtin + custom)'
        'create:Create a new profile (TUI)'
    )

    shells=(bash zsh fish)
    tiers=(cheap balanced premium)

    case "${words[2]}" in
        profile)
            if (( CURRENT == 3 )); then
                # Complete with subcommands + profile names
                local -a profiles
                profiles=(${(f)"$(skynex profile list 2>/dev/null | grep -E '^\s+\S+' | awk '{print $1}' | grep -v '^Built-in$|^Custom$|^No$|^Create$|^Default:|^Usage:')"})
                _describe 'profile command' profile_subcommands
                compadd -a tiers
                compadd -a profiles
            elif (( CURRENT == 4 )); then
                local -a verbs
                verbs=(edit delete default)
                compadd -a verbs
            fi
            ;;
        up)
            if (( CURRENT == 3 )); then
                local -a profiles
                profiles=(${(f)"$(skynex profile list 2>/dev/null | grep -E '^\s+\S+' | awk '{print $1}' | grep -v '^Built-in$|^Custom$|^No$|^Create$|^Default:|^Usage:')"})
                compadd -a tiers
                compadd -a profiles
                compadd -- --web --port
            else
                compadd -- --web --port
            fi
            ;;
        completion)
            compadd -a shells
            ;;
        *)
            _describe 'command' commands
            ;;
    esac
}

_skynex "$@"
`
}

// Fish returns a fish completion script for skynex.
func Fish() string {
	return `# skynex fish completion

# Disable file completions for skynex
complete -c skynex -f

# Top-level commands
complete -c skynex -n "__fish_use_subcommand" -a install -d "Interactive installer (TUI)"
complete -c skynex -n "__fish_use_subcommand" -a doctor -d "Check environment and dependencies"
complete -c skynex -n "__fish_use_subcommand" -a version -d "Show version"
complete -c skynex -n "__fish_use_subcommand" -a profile -d "Manage profiles"
complete -c skynex -n "__fish_use_subcommand" -a up -d "Launch OpenCode with a profile"
complete -c skynex -n "__fish_use_subcommand" -a completion -d "Generate shell completion script"
complete -c skynex -n "__fish_use_subcommand" -a status -d "Show environment status"
complete -c skynex -n "__fish_use_subcommand" -a update -d "Update installed packages"

# profile subcommands
complete -c skynex -n "__fish_seen_subcommand_from profile" -a list -d "List all profiles"
complete -c skynex -n "__fish_seen_subcommand_from profile" -a create -d "Create a new profile"
complete -c skynex -n "__fish_seen_subcommand_from profile" -a "cheap balanced premium" -d "Built-in tier"

# profile <name> verbs
complete -c skynex -n "__fish_seen_subcommand_from profile; and __fish_is_nth_token 4" -a edit -d "Edit profile"
complete -c skynex -n "__fish_seen_subcommand_from profile; and __fish_is_nth_token 4" -a delete -d "Delete profile"
complete -c skynex -n "__fish_seen_subcommand_from profile; and __fish_is_nth_token 4" -a default -d "Set as default"

# up options
complete -c skynex -n "__fish_seen_subcommand_from up" -a "cheap balanced premium" -d "Built-in tier"
complete -c skynex -n "__fish_seen_subcommand_from up" -l web -d "Launch web UI"
complete -c skynex -n "__fish_seen_subcommand_from up" -l port -d "Specify port" -r

# completion shells
complete -c skynex -n "__fish_seen_subcommand_from completion" -a "bash zsh fish" -d "Shell type"

# install options
complete -c skynex -n "__fish_seen_subcommand_from install" -l package -d "Package to install" -r
complete -c skynex -n "__fish_seen_subcommand_from install" -l target -d "Target: claude, opencode, both" -r
complete -c skynex -n "__fish_seen_subcommand_from install" -l version -d "Package version" -r
complete -c skynex -n "__fish_seen_subcommand_from install" -l non-interactive -d "Skip prompts"
complete -c skynex -n "__fish_seen_subcommand_from install" -s y -l yes -d "Skip confirmation"
complete -c skynex -n "__fish_seen_subcommand_from install" -l verbose -d "Show detailed install progress"
`
}
