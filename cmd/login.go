package cmd

import (
	"fmt"
)

// LoginCmd prints instructions for capturing a DeepSeek session: the bearer
// token from localStorage.userToken and the ds_session_id cookie.
type LoginCmd struct{}

func (c *LoginCmd) Run(app *App) error {
	fmt.Println(`dscli uses your signed-in chat.deepseek.com session (free account, no API key).`)
	fmt.Println()
	fmt.Println(`1. Open https://chat.deepseek.com in a browser and sign in.`)
	fmt.Println(`2. Open the developer console (F12 -> Console) and paste:`) 
	fmt.Println()
	fmt.Println(loginSnippet())
	fmt.Println()
	fmt.Println(`3. It prints a JSON object with "token" and "cookie" values. Save them:`)
	fmt.Println()
	fmt.Println(`   dscli config set token "<token>"`)
	fmt.Println(`   dscli config set cookie "<cookie>"`)
	fmt.Println()
	fmt.Println(`The values live only in your config file (~/.config/dscli/dscli.json)`)
	fmt.Println(`and can be rotated any time; alternatively set DS_TOKEN / DS_COOKIE env vars.`)
	return nil
}

// loginSnippet is a console one-liner for the signed-in site. It reads the
// bearer token the web app keeps in localStorage.userToken and pulls the
// ds_session_id cookie.
func loginSnippet() string {
	return `(() => { const t = JSON.parse(localStorage.getItem('userToken') || '{}').value;
const m = document.cookie.match(/(?:^|; )ds_session_id=([^;]+)/);
console.log(JSON.stringify({ token: t || null, cookie: m ? m[1] : null })); })();`
}