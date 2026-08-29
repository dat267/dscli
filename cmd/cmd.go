package cmd

// CLI is the root CLI struct containing all subcommand groups.
type CLI struct {
	ConfigFile     string            `help:"Config file path" json:"-"`
	Chat           ChatCmd           `cmd:"" help:"Chat with DeepSeek (omit the prompt for an interactive session)"`
	Ask            AskCmd            `cmd:"" help:"Ask the model once and print the answer (input from args or stdin)"`
	Translate      TranslateCmd      `cmd:"" help:"Translate a file (txt, md, lrc, srt, vtt, ass, ttml, epub) via the model"`
	ImproveWriting ImproveWritingCmd `cmd:"" help:"Improve the writing of a file in place (txt, md, lrc, srt, vtt, ass, ttml) via the model"`
	Session        SessionCmdGroup   `cmd:"" help:"Inspect, forget or delete the persisted default session"`
	Login          LoginCmd          `cmd:"" help:"Show how to capture your DeepSeek login (token + cookie)"`
	Version        VersionCmd        `cmd:"" help:"Show version"`
	Config         ConfigCmdGroup    `cmd:"" help:"Manage application configuration"`
}
