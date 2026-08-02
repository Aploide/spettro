//go:build windows

package notify

import (
	"encoding/base64"
	"os"
	"os/exec"
	"unicode/utf16"
)

// toastAppID is the Application User Model ID the notification is published
// under. Windows refuses to show a toast from an AppID that has no registered
// Start menu entry, and spettro is a portable console binary with no installer
// to register one. Borrowing the Windows PowerShell shortcut's AppID — present
// on every supported Windows release — is what makes an unregistered CLI able
// to toast at all; the cost is that the banner is attributed to PowerShell.
const toastAppID = `{1AC14E77-02E7-4E5D-B744-2EB1AE5198B7}\WindowsPowerShell\v1.0\powershell.exe`

// toastScript builds the toast from the WinRT notification API. Title and body
// arrive through the environment rather than being interpolated into the
// script: they carry tool output and model text, so pasting them into source
// would let a stray quote break the script and let crafted text run arbitrary
// PowerShell. CreateTextNode escapes them into the toast XML.
const toastScript = `$ErrorActionPreference = 'Stop'
$appId = '` + toastAppID + `'
[void][Windows.UI.Notifications.ToastNotificationManager, Windows.UI.Notifications, ContentType=WindowsRuntime]
[void][Windows.Data.Xml.Dom.XmlDocument, Windows.Data.Xml.Dom, ContentType=WindowsRuntime]
$t = [Windows.UI.Notifications.ToastNotificationManager]::GetTemplateContent([Windows.UI.Notifications.ToastTemplateType]::ToastText02)
$n = $t.GetElementsByTagName('text')
[void]$n.Item(0).AppendChild($t.CreateTextNode($env:SPETTRO_NOTIFY_TITLE))
[void]$n.Item(1).AppendChild($t.CreateTextNode($env:SPETTRO_NOTIFY_BODY))
$toast = [Windows.UI.Notifications.ToastNotification]::new($t)
[Windows.UI.Notifications.ToastNotificationManager]::CreateToastNotifier($appId).Show($toast)
`

// sendPlatform raises a Windows toast notification.
//
// It always runs powershell.exe, never pwsh: the WinRT projection these types
// rely on is built into Windows PowerShell 5.1, while PowerShell 7 needs a
// separate interop assembly that is not installed by default.
func sendPlatform(title, body string) { startAndReap(toastCommand(title, body)) }

// toastCommand builds the notification process. It is separate from
// sendPlatform so tests can run it synchronously and assert the WinRT script
// succeeds, which a fire-and-forget Start would silently hide.
func toastCommand(title, body string) *exec.Cmd {
	cmd := exec.Command("powershell.exe",
		"-NoLogo", "-NoProfile", "-NonInteractive",
		"-ExecutionPolicy", "Bypass",
		"-EncodedCommand", encodeUTF16LE(toastScript),
	)
	cmd.Env = append(os.Environ(),
		"SPETTRO_NOTIFY_TITLE="+title,
		"SPETTRO_NOTIFY_BODY="+body,
	)
	return cmd
}

// encodeUTF16LE renders the script as the base64 UTF-16LE blob that
// -EncodedCommand expects, which keeps the multi-line script intact through
// argument parsing.
func encodeUTF16LE(s string) string {
	units := utf16.Encode([]rune(s))
	buf := make([]byte, 0, len(units)*2)
	for _, u := range units {
		buf = append(buf, byte(u), byte(u>>8))
	}
	return base64.StdEncoding.EncodeToString(buf)
}
