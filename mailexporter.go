package main

import (
	"bytes"
	"errors"
	"flag"
	"io"
	"io/ioutil"
	"log"
	"math/rand"
	"net/http"
	"net/mail"
	"net/smtp"
	"os"
	"strconv"
	"strings"
	"time"

	auth "github.com/abbot/go-http-auth"
	"github.com/prometheus/client_golang/prometheus"
	promlog "github.com/prometheus/log"
	"gopkg.in/fsnotify.v1"
	"gopkg.in/yaml.v2"
)

var tokenLength = 40 // length of token for probing-mails

// holds a configuration of external server to send test mails
var globalconf struct {
	// The path to the TLS-Public-Key.
	CrtPath string
	// The path to the TLS-Private-Key.
	KeyPath string
	// The username for HTTP Basic Auth.
	AuthUser string
	// The passphrase for HTTP Basic Auth.
	AuthPass string
	// The hashvalue to be used in HTTP Basic Auth (filled in parseConfig).
	authHash string
	// The port to listen on for Prometheus-Endpoint.
	HTTPPort string
	// The URL for prometheus' metrics-endpoint.
	HTTPEndpoint string

	// The time to wait between probe-attempts.
	MonitoringInterval time.Duration
	// The time between start of monitoring-goroutines.
	StartupOffset time.Duration
	// The time to wait until mail_deliver_success = 0 is reported.
	MailCheckTimeout time.Duration

	// SMTP-Servers used for probing.
	Servers []SMTPServerConfig
}

type SMTPServerConfig struct {
	// The name the probing attempts via this server are classified with.
	Name string
	// The address of the SMTP-server.
	Server string
	// The port of the SMTP-server.
	Port string
	// The username for the SMTP-server.
	Login string
	// The SMTP-user's passphrase.
	Passphrase string
	// The sender-address for the probing mails.
	From string
	// The destination the probing-mails are sent to.
	To string
	// The directory in which mails sent by this server will end up if delivered correctly.
	Detectiondir string
}

var (
	// cli-flags
	confPath = flag.String("config-file", "./mailexporter.conf", "config-file to use")
	useTLS   = flag.Bool("tls", true, "use TLS for metrics-endpoint")
	useAuth  = flag.Bool("auth", true, "use HTTP-Basic-Auth for metrics-endpoint")

	// errors
	ErrMailNotFound = errors.New("no corresponding mail found")
	ErrNotOurDept   = errors.New("no mail of ours")
)

// holds information about probing-email with the corresponding file name
type email struct {
	// filename of the mailfile
	Filename string
	// name of the configuration the mail originated from
	Name string
	// unique token to identify the mail even if timings and name are exactly the same
	Token string
	// time the mail was sent as unix-timestamp
	T_sent int64
	// time the mail was detected as unix-timestamp
	T_recv int64
}

// prometheus-instrumentation

var deliver_ok = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "mail_deliver_success",
		Help: "indicatior whether last mail was delivered successfully",
	},
	[]string{"configname"},
)

var last_mail_deliver_time = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "last_mail_deliver_time",
		Help: "timestamp (in s) of detection of last correctly received testmail",
	},
	[]string{"configname"},
)

var last_mail_deliver_duration = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "last_mail_deliver_duration",
		Help: "duration (in ms) of delivery of last correctly received testmail",
	},
	[]string{"configname"},
)

var late_mails = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "late_mails",
		Help: "number of probing-mails received after their respective timeout",
	},
	[]string{"configname"},
)

var mail_deliver_durations = prometheus.NewHistogramVec(
	prometheus.HistogramOpts{
		Name:    "mail_deliver_durations",
		Help:    "durations (in ms) of mail delivery",
		Buckets: histBuckets(100e3, 50),
	},
	[]string{"configname"},
)

// histBuckets returns a linearly spaced []float64 to be used as Buckets in a prometheus.Histogram.
func histBuckets(upperBound float64, binSize float64) []float64 {
	bins := int(upperBound) / int(binSize)

	buckets := make([]float64, bins)
	binBorder := binSize
	for i := 0; i < bins; i++ {
		buckets[i] = binBorder
		binBorder += binSize
	}
	return buckets
}

func init() {
	prometheus.MustRegister(deliver_ok)
	prometheus.MustRegister(last_mail_deliver_time)
	prometheus.MustRegister(late_mails)
	prometheus.MustRegister(last_mail_deliver_duration)
	prometheus.MustRegister(mail_deliver_durations)
}

// parseConfig parses configuration file and tells us if we are ready to rumble.
func parseConfig(r io.Reader) error {
	content, err := ioutil.ReadAll(r)
	if err != nil {
		return err
	}

	err = yaml.Unmarshal(content, &globalconf)
	if err != nil {
		return err
	}

	// the basic HTTP-Auth-Lib doesn't support Plaintext-passwords up to now, therefore precompute an md5-hash for that
	globalconf.authHash = string(auth.MD5Crypt([]byte(globalconf.AuthPass), []byte("salt"), []byte("$magic$")))

	return nil
}

// send sends a probing-email over SMTP-server specified in config c to be waited for on the receiving side.
func send(c SMTPServerConfig, msg string) {
	promlog.Debug("sending mail")
	a := smtp.PlainAuth("", c.Login, c.Passphrase, c.Server)
	err := smtp.SendMail(c.Server+":"+c.Port, a, c.From, []string{c.To}, []byte(msg))

	if err != nil {
		promlog.Warn(err)
	}
}

// generateToken returns a random string to pad the send mail with for identifying
// it later in the maildir (and not mistake another one for it)
// does also return unprintable characters in returned string,
// which is actually appreciated to implicitly monitor that
// mail gets through unchanged
// although, if you would print those unprintable characters,
// be aware of funny side-effects like terminal commands being
// triggered and stuff like that, therefore use
// fmt.Printf("%q", unprintableString) for that
func generateToken(length int) string {
	stuff := make([]byte, length)

	for i := range stuff {
		stuff[i] = byte(rand.Int())
	}

	rstr := string(stuff)

	// ensure no "-" are in the returned string
	// as "-" is used later as a field splitter
	rstr = strings.Replace(rstr, "-", "X", -1)

	// ensure no ":" are in the returned string
	// otherwise our payload might be made a header
	// instead of the mailbody by mail.Send()
	rstr = strings.Replace(rstr, ":", "X", -1)

	return rstr
}

// deleteMail delete the given mail to not leave an untidied maildir.
func deleteMail(m email) {
	if err := os.Remove(m.Filename); err != nil {
		promlog.Warn(err)
	}
	promlog.Debug("rm ", m.Filename)
}

// composePayload composes a payload to be used in probing mails for identification
// consisting of config name, unix time and a unique token for identification.
func composePayload(name string, unixtimestamp int64) (payload string, token string) {
	timestampstr := strconv.FormatInt(unixtimestamp, 10)

	// Now get the token to have a unique token.
	token = generateToken(tokenLength)

	payload = strings.Join([]string{name, token, timestampstr}, "-")
	promlog.Debug("composed payload:", payload)

	return payload, token
}

// decomposePayload returns the config name and unix timestamp as appropriate types
// from given payload.
func decomposePayload(payload []byte) (name string, token string, extractedUnixTime int64, err error) {
	promlog.Debug("payload to decompose:", payload)

	decomp := strings.Split(string(payload), "-")
	// is it correctly parsable?
	if len(decomp) != 3 {
		promlog.Debug("no fitting decomp")
		return "", "", -1, ErrNotOurDept
	}

	extractedUnixTime, err = strconv.ParseInt(decomp[2], 10, 64)
	// is the last one a unix-timestamp?
	if err != nil {
		promlog.Debug("unix-timestamp-parse-error")
		return "", "", -1, ErrNotOurDept
	}

	return decomp[0], decomp[1], extractedUnixTime, nil
}

// lateMail logs mails that have been so late that they timed out
func lateMail(m email) {
	promlog.Debug("got late mail via", m.Name)
	late_mails.WithLabelValues(m.Name).Inc()
}

// probe probes if mail gets through the entire chain from specified SMTPServer into Maildir.
// the argument "reportChans" contains channels to each monitoring goroutine where to drop
// the found mails into.
func probe(c SMTPServerConfig, reportChan chan email) {
	payload, token := composePayload(c.Name, time.Now().UnixNano())
	send(c, payload)

	timeout := time.After(globalconf.MailCheckTimeout)

	// the seekloop is needed to account for mails that are coming late.
	// Otherwise, a late mail would trigger the first case and stop us from
	// being able to detect the mail we are actually waiting for

seekloop:
	for {
		select {
		case mail := <-reportChan:
			promlog.Debug("getting mail...")

			// timestamps are in nanoseconds
			// last_mail_deliver_time shall be standard unix-timestamp
			// last_mail_deliver_duration shall be milliseconds for higher resolution
			deliverTime := float64(mail.T_recv / int64(time.Second))
			deliverDuration := float64((mail.T_recv - mail.T_sent) / int64(time.Millisecond))
			last_mail_deliver_time.WithLabelValues(c.Name).Set(deliverTime)
			last_mail_deliver_duration.WithLabelValues(c.Name).Set(deliverDuration)
			mail_deliver_durations.WithLabelValues(c.Name).Observe(deliverDuration)

			if mail.Token == token {
				// we obtained the expected mail
				deliver_ok.WithLabelValues(c.Name).Set(1)
				deleteMail(mail)
				break seekloop

			}
			// it was another mail with unfitting token, probably late
			lateMail(mail)

		case <-timeout:
			promlog.Debug("Getting mail timed out.")
			deliver_ok.WithLabelValues(c.Name).Set(0)
			break seekloop
		}
	}
}

// monitor probes every MonitoringInterval if mail still gets through.
func monitor(c SMTPServerConfig, reportChan chan email) {
	log.Println("Started monitoring for config", c.Name)
	for {
		probe(c, reportChan)
		time.Sleep(globalconf.MonitoringInterval)
	}
}

// secret returns secret for basic http-auth
func secret(user, realm string) string {
	if user == globalconf.AuthUser {
		return globalconf.authHash
	}
	return ""
}

// detectMail monitors Detectiondirs and reports mails that come in.
func detectMail(watcher *fsnotify.Watcher, reportChans map[string]chan email) {
	log.Println("Started mail-detection.")

	for {
		select {
		case event := <-watcher.Events:
			if event.Op&fsnotify.Create == fsnotify.Create {
				if mail, err := parseMail(event.Name); err == nil {
					reportChans[mail.Name] <- mail
				}
			}
		case err := <-watcher.Errors:
			promlog.Warn("watcher-error:", err)
		}
	}
}

// parseMail reads a mailfile's content and parses it into a mail-struct if one of ours.
func parseMail(path string) (email, error) {
	// to date the mails found
	t := time.Now().UnixNano()

	// try parsing
	f, err := os.Open(path)
	if err != nil {
		return email{}, err
	}
	defer f.Close()

	mail, err := mail.ReadMessage(io.LimitReader(f, 8192))
	if err != nil {
		return email{}, err
	}

	payload, err := ioutil.ReadAll(mail.Body)
	if err != nil {
		return email{}, err
	}
	payload = bytes.TrimSpace(payload) // mostly for trailing "\n"

	name, token, unixtime, err := decomposePayload(payload)
	// return if parsable
	// (non-parsable mails are not sent by us (or broken) and therefore not needed
	if err != nil {
		return email{}, ErrNotOurDept
	}

	return email{path, name, token, unixtime, t}, nil
}

func main() {
	flag.Parse()

	// seed the RNG, otherwise we would have same randomness on every startup
	// which should not, but might in worst case interfere with leftover-mails
	// from earlier starts of the binary
	rand.Seed(time.Now().Unix())

	f, err := os.Open(*confPath)
	if err != nil {
		promlog.Fatal(err)
	}

	err = parseConfig(f)
	f.Close()
	if err != nil {
		promlog.Fatal(err)
	}

	// initialize Metrics that will be used seldom so that they actually get exported with a metric
	for _, c := range globalconf.Servers {
		late_mails.GetMetricWithLabelValues(c.Name)
	}

	fswatcher, err := fsnotify.NewWatcher()
	if err != nil {
		promlog.Fatal(err)
	}

	defer fswatcher.Close()

	// reportChans will be used to send found mails from the detection-goroutine to the monitoring-goroutines
	reportChans := make(map[string]chan email)

	for _, c := range globalconf.Servers {
		fswatcher.Add(c.Detectiondir) // deduplication is done within fsnotify
		reportChans[c.Name] = make(chan email)
		promlog.Debug("adding path to watcher:", c.Detectiondir)
	}

	go detectMail(fswatcher, reportChans)

	// now fire up the monitoring jobs
	for _, c := range globalconf.Servers {
		go monitor(c, reportChans[c.Name])

		// keep a timedelta between monitoring jobs to reduce interference
		// (although that shouldn't be an issue)
		time.Sleep(globalconf.StartupOffset)
	}

	log.Println("Starting HTTP-endpoint")
	if *useAuth {
		authenticator := auth.NewBasicAuthenticator("prometheus", secret)
		http.HandleFunc(globalconf.HTTPEndpoint, auth.JustCheck(authenticator, prometheus.Handler().ServeHTTP))
	} else {
		http.Handle(globalconf.HTTPEndpoint, prometheus.Handler())
	}

	if *useTLS {
		promlog.Fatal(http.ListenAndServeTLS(":"+globalconf.HTTPPort, globalconf.CrtPath, globalconf.KeyPath, nil))
	} else {
		promlog.Fatal(http.ListenAndServe(":"+globalconf.HTTPPort, nil))
	}
}
