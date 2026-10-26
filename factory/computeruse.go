package factory

// computerUseRule is shared by Fellows and UI reviewers. Connection names and host settings
// stay in the current skill, outside the repo. No format verbs: newCardPrompt uses Sprintf.
const computerUseRule = `Computer use through PDW:
- Check ` + "`command -v pdw`" + ` once. Read start-here in full once per session with
  ` + "`pdw call skills__skill_read --data '{\"name\":\"start-here\"}'`" + `, unless already read.
- For assigned web research, browsing or UI checks, read the current computer-use skill in full first:
  ` + "`pdw call skills__skill_read --data '{\"name\":\"openai-computer-use\"}'`" + `.
  Discover the connection and tool names there; use ` + "`pdw describe`" + ` for arguments and ` + "`pdw call`" + ` to run them.
  Computer use is available through PDW across agent providers and does not require a local desktop.
  Follow the skill's browser route, not Paseo browser tools or a throwaway headless browser script.
- Do not open a browser at startup. Open one only when the assigned work needs it; avoid parallel sessions.
  Follow the documented first-call entry point and read its returned API documentation. Reuse the tab
  binding across separate shell calls in the same agent; follow the skill's recovery steps if it is lost.
- For a UI check, run the version under test on test or fake data, or through a dry-run mode the repo's README documents
  (one that changes nothing). Never test mutations against the live service. Bind the test server to this
  host's tailnet IP from ` + "`tailscale ip -4`" + `, not a loopback or wildcard address or hostname output,
  so the remote browser can reach it. A local server address is not a link the user can necessarily open.
- Close every tab you opened or claimed and stop every server you started before your final message.
  Verify cleanup using your own session's state; never close another agent's or the user's tabs or apps.
- Tool access does not authorize purchases, sends, submissions, production changes or other actions outside
  the assigned scope. Keep this role's read-only limits and existing approval gates. Do not change host settings,
  install software or restart services to repair access. Report the failed step and incomplete verification;
  do not claim computer use is unavailable merely because this agent runs on a different host.
`

const fellowComputerUseRule = computerUseRule + `
This applies on the direct track too, including research and checking a site.
Click through your own UI changes with computer use before code review: test every changed flow,
read the page text and inspect screenshots at desktop and phone widths. Use the safe test setup above.
If access fails or the gateway is full, report what you tried and what remains unverified; never call it a pass.
`
