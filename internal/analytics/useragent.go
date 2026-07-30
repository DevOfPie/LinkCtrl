package analytics

import "strings"

// Device, browser and OS classification.
//
// Hand-written rather than a user-agent library on purpose. The libraries are
// large, carry regularly-updated regex databases, and answer a much harder
// question than this needs. Analytics here reports coarse buckets — is this a
// phone or a desktop, roughly which browser — and a few dozen substring checks
// answer that at a fraction of the cost, on a path that runs for every click.
//
// The trade is accuracy on unusual agents. That is acceptable for bucketed
// reporting and is stated in the UI rather than implied away.

type Device string

const (
	DeviceDesktop Device = "desktop"
	DeviceMobile  Device = "mobile"
	DeviceTablet  Device = "tablet"
	DeviceBot     Device = "bot"
	DeviceUnknown Device = "unknown"
)

// Classification is everything derived from a user agent.
type Classification struct {
	Device  Device
	Browser string
	OS      string
	IsBot   bool
}

// botMarkers are substrings that identify automated traffic.
//
// Recording bots but excluding them from default charts is the honest choice:
// dropping them entirely loses the ability to explain a traffic spike, while
// counting them makes a Slack unfurl look like a real visitor.
var botMarkers = []string{
	"bot", "crawler", "spider", "crawling",
	"slurp", "mediapartners", "adsbot", "feedfetcher",
	"curl/", "wget/", "python-requests", "python-urllib", "go-http-client",
	"java/", "okhttp", "axios/", "node-fetch", "got (", "libwww-perl",
	"httpclient", "postmanruntime", "insomnia", "restsharp",
	"headlesschrome", "phantomjs", "puppeteer", "playwright", "selenium",
	"lighthouse", "pagespeed", "gtmetrix", "pingdom", "uptimerobot",
	"statuscake", "site24x7", "newrelicpinger", "datadog",
	// Link unfurlers. Common on a shortener and definitely not human visits.
	"facebookexternalhit", "twitterbot", "linkedinbot", "slackbot",
	"discordbot", "telegrambot", "whatsapp", "skypeuripreview",
	"embedly", "quora link preview", "redditbot", "applebot",
	"pinterest", "vkshare", "w3c_validator", "developers.google.com/+/web/snippet",
	"preview", "scanner", "monitor", "checker", "validator",
}

// Classify buckets a user agent.
func Classify(ua string) Classification {
	if ua == "" {
		// An absent user agent is almost always tooling, not a browser.
		return Classification{Device: DeviceBot, Browser: "", OS: "", IsBot: true}
	}

	lower := strings.ToLower(ua)

	for _, marker := range botMarkers {
		if strings.Contains(lower, marker) {
			return Classification{Device: DeviceBot, Browser: browserOf(lower), OS: osOf(lower), IsBot: true}
		}
	}

	return Classification{
		Device:  deviceOf(lower),
		Browser: browserOf(lower),
		OS:      osOf(lower),
		IsBot:   false,
	}
}

func deviceOf(lower string) Device {
	switch {
	case strings.Contains(lower, "ipad"),
		strings.Contains(lower, "tablet"),
		strings.Contains(lower, "kindle"),
		strings.Contains(lower, "playbook"),
		// Android without "mobile" is conventionally a tablet.
		strings.Contains(lower, "android") && !strings.Contains(lower, "mobile"):
		return DeviceTablet
	case strings.Contains(lower, "mobi"),
		strings.Contains(lower, "iphone"),
		strings.Contains(lower, "ipod"),
		strings.Contains(lower, "android"),
		strings.Contains(lower, "windows phone"),
		strings.Contains(lower, "blackberry"):
		return DeviceMobile
	case strings.Contains(lower, "mozilla"), strings.Contains(lower, "webkit"):
		return DeviceDesktop
	default:
		return DeviceUnknown
	}
}

// browserOf checks in order of specificity. Order is load-bearing: Edge and
// Opera both claim Chrome, and Chrome claims Safari, so testing for the
// general case first would misreport every one of them.
func browserOf(lower string) string {
	switch {
	case strings.Contains(lower, "edg/"), strings.Contains(lower, "edga/"), strings.Contains(lower, "edgios/"):
		return "Edge"
	case strings.Contains(lower, "opr/"), strings.Contains(lower, "opera"):
		return "Opera"
	case strings.Contains(lower, "vivaldi"):
		return "Vivaldi"
	case strings.Contains(lower, "brave"):
		return "Brave"
	case strings.Contains(lower, "samsungbrowser"):
		return "Samsung Internet"
	case strings.Contains(lower, "firefox"), strings.Contains(lower, "fxios"):
		return "Firefox"
	case strings.Contains(lower, "chrome"), strings.Contains(lower, "crios"):
		return "Chrome"
	case strings.Contains(lower, "safari"):
		return "Safari"
	case strings.Contains(lower, "msie"), strings.Contains(lower, "trident"):
		return "Internet Explorer"
	default:
		return "Other"
	}
}

func osOf(lower string) string {
	switch {
	case strings.Contains(lower, "windows nt"), strings.Contains(lower, "windows"):
		return "Windows"
	// Before the generic mac check: iOS agents also contain "mac os x".
	case strings.Contains(lower, "iphone"), strings.Contains(lower, "ipad"), strings.Contains(lower, "ipod"):
		return "iOS"
	case strings.Contains(lower, "mac os x"), strings.Contains(lower, "macintosh"):
		return "macOS"
	// Before Linux: Android agents contain "linux".
	case strings.Contains(lower, "android"):
		return "Android"
	case strings.Contains(lower, "cros"):
		return "ChromeOS"
	case strings.Contains(lower, "linux"), strings.Contains(lower, "x11"):
		return "Linux"
	default:
		return "Other"
	}
}

// PrimaryLanguage extracts the first tag from an Accept-Language header.
//
// Only the language subtag is kept ("en" from "en-GB"). Region adds
// granularity nobody reports on and narrows the anonymity set.
func PrimaryLanguage(header string) string {
	if header == "" {
		return ""
	}
	first, _, _ := strings.Cut(header, ",")
	first, _, _ = strings.Cut(first, ";")
	first = strings.TrimSpace(first)
	if lang, _, ok := strings.Cut(first, "-"); ok {
		first = lang
	}
	if len(first) > 8 {
		return ""
	}
	return strings.ToLower(first)
}

// ReferrerHost extracts the host from a referrer.
//
// Only the host is kept. Full referrer URLs routinely carry query parameters
// with session tokens, search terms and personal data, so the rest is
// discarded at the edge rather than stored and cleaned up later.
func ReferrerHost(referrer string) string {
	if referrer == "" {
		return ""
	}
	s := referrer
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if i := strings.IndexAny(s, "/?#"); i >= 0 {
		s = s[:i]
	}
	if i := strings.LastIndex(s, "@"); i >= 0 {
		s = s[i+1:] // strip any userinfo
	}
	if i := strings.LastIndex(s, ":"); i >= 0 && !strings.Contains(s, "]") {
		s = s[:i] // strip the port, leaving bracketed IPv6 alone
	}
	s = strings.ToLower(strings.TrimSpace(s))
	if len(s) > 253 {
		return ""
	}
	return s
}
