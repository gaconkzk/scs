package scs

import (
	"bufio"
	"bytes"
	"log"
	"net"
	"net/http"
	"time"

	"github.com/gaconkzk/scs/v2/memstore"
)

// Session Deprecated: Session is a backwards-compatible alias for SessionManager.
type Session = SessionManager

// SessionManager holds the configuration settings for your sessions.
type SessionManager struct {
	// IdleTimeout controls the maximum length of time a session can be inactive
	// before it expires. For example, some applications may wish to set this so
	// there is a timeout after 20 minutes of inactivity. By default IdleTimeout
	// is not set and there is no inactivity timeout.
	IdleTimeout time.Duration

	// Lifetime controls the maximum length of time that a session is valid for
	// before it expires. The lifetime is an 'absolute expiry' which is set when
	// the session is first created and does not change. The default value is 24
	// hours.
	Lifetime time.Duration

	// Store controls the session store where the session data is persisted.
	Store Store

	// Cookie contains the configuration settings for session cookies.
	Cookie SessionCookie

	// Codec controls the encoder/decoder used to transform session data to a
	// byte slice for use by the session store. By default session data is
	// encoded/decoded using encoding/gob.
	Codec Codec

	// ErrorFunc allows you to control behavior when an error is encountered by
	// the LoadAndSave middleware. The default behavior is for a HTTP 500
	// "Internal Server Error" message to be sent to the client and the error
	// logged using Go's standard logger. If a custom ErrorFunc is set, then
	// control will be passed to this instead. A typical use would be to provide
	// a function which logs the error and returns a customized HTML error page.
	ErrorFunc func(http.ResponseWriter, *http.Request, error)

	// contextKey is the key used to set and retrieve the session data from a
	// context.Context. It's automatically generated to ensure uniqueness.
	contextKey contextKey
}

// SessionCookie contains the configuration settings for session cookies.
type SessionCookie struct {
	// Name sets the name of the session cookie. It should not contain
	// whitespace, commas, colons, semicolons, backslashes, the equals sign or
	// control characters as per RFC6265. The default cookie name is "session".
	// If your application uses two different sessions, you must make sure that
	// the cookie name for each is unique.
	Name string

	// Domain sets the 'Domain' attribute on the session cookie. By default
	// it will be set to the domain name that the cookie was issued from.
	Domain string

	// HTTPOnly sets the 'HTTPOnly' attribute on the session cookie. The
	// default value is true.
	HTTPOnly bool

	// Path sets the 'Path' attribute on the session cookie. The default value
	// is "/". Passing the empty string "" will result in it being set to the
	// path that the cookie was issued from.
	Path string

	// Persist sets whether the session cookie should be persistent or not
	// (i.e. whether it should be retained after a user closes their browser).
	// The default value is true, which means that the session cookie will not
	// be destroyed when the user closes their browser and the appropriate
	// 'Expires' and 'MaxAge' values will be added to the session cookie. If you
	// want to only persist some sessions (rather than all of them), then set this
	// to false and call the RememberMe() method for the specific sessions that you
	// want to persist.
	Persist bool

	// SameSite controls the value of the 'SameSite' attribute on the session
	// cookie. By default this is set to 'SameSite=Lax'. If you want no SameSite
	// attribute or value in the session cookie then you should set this to 0.
	SameSite http.SameSite

	// Secure sets the 'Secure' attribute on the session cookie. The default
	// value is false. It's recommended that you set this to true and serve all
	// requests over HTTPS in production environments.
	// See https://github.com/OWASP/CheatSheetSeries/blob/master/cheatsheets/Session_Management_Cheat_Sheet.md#transport-layer-security.
	Secure bool
}

// New returns a new session manager with the default options. It is safe for
// concurrent use.
func New() *SessionManager {
	s := &SessionManager{
		IdleTimeout: 0,
		Lifetime:    24 * time.Hour,
		Store:       memstore.New(),
		Codec:       GobCodec{},
		ErrorFunc:   defaultErrorFunc,
		contextKey:  generateContextKey(),
		Cookie: SessionCookie{
			Name:     "session",
			Domain:   "",
			HTTPOnly: true,
			Path:     "/",
			Persist:  true,
			Secure:   false,
			SameSite: http.SameSiteLaxMode,
		},
	}
	return s
}

// NewSession Deprecated:  is a backwards-compatible alias for New. Use the New
// function instead.
func NewSession() *SessionManager {
	return New()
}

// LoadAndSave provides middleware which automatically loads and saves session
// data for the current request, and communicates the session token to and from
// the client in a cookie.
func (s *SessionManager) LoadAndSave(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var token string
		cookie, err := r.Cookie(s.Cookie.Name)
		if err == nil {
			token = cookie.Value
		}

		ctx, err := s.Load(r.Context(), token)
		if err != nil {
			s.ErrorFunc(w, r, err)
			return
		}

		sr := r.WithContext(ctx)
		bw := &bufferedResponseWriter{ResponseWriter: w}
		next.ServeHTTP(bw, sr)

		if sr.MultipartForm != nil {
			sr.MultipartForm.RemoveAll()
		}

		if s.Status(ctx) != Unmodified {
			responseCookie := &http.Cookie{
				Name:     s.Cookie.Name,
				Path:     s.Cookie.Path,
				Secure:   s.Cookie.Secure,
				HttpOnly: s.Cookie.HTTPOnly,
				SameSite: s.Cookie.SameSite,
			}
			if s.Cookie.Domain != "" {
				responseCookie.Domain = s.Cookie.Domain
			}

			switch s.Status(ctx) {
			case Modified:
				token, expiry, err := s.Commit(ctx)
				if err != nil {
					s.ErrorFunc(w, r, err)
					return
				}

				responseCookie.Value = token

				if s.Cookie.Persist || s.GetBool(ctx, "__rememberMe") {
					responseCookie.Expires = time.Unix(expiry.Unix()+1, 0)        // Round up to the nearest second.
					responseCookie.MaxAge = int(time.Until(expiry).Seconds() + 1) // Round up to the nearest second.
				}
			case Destroyed:
				responseCookie.Expires = time.Unix(1, 0)
				responseCookie.MaxAge = -1
			}

			w.Header().Add("Set-Cookie", responseCookie.String())
			addHeaderIfMissing(w, "Cache-Control", `no-cache="Set-Cookie"`)
			addHeaderIfMissing(w, "Vary", "Cookie")
		}

		if bw.code != 0 {
			w.WriteHeader(bw.code)
		}
		w.Write(bw.buf.Bytes())
	})
}

func addHeaderIfMissing(w http.ResponseWriter, key, value string) {
	for _, h := range w.Header()[key] {
		if h == value {
			return
		}
	}
	w.Header().Add(key, value)
}

func defaultErrorFunc(w http.ResponseWriter, r *http.Request, err error) {
	log.Output(2, err.Error())
	http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
}

type bufferedResponseWriter struct {
	http.ResponseWriter
	buf         bytes.Buffer
	code        int
	wroteHeader bool
}

func (bw *bufferedResponseWriter) Write(b []byte) (int, error) {
	return bw.buf.Write(b)
}

func (bw *bufferedResponseWriter) WriteHeader(code int) {
	if !bw.wroteHeader {
		bw.code = code
		bw.wroteHeader = true
	}
}

func (bw *bufferedResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hj := bw.ResponseWriter.(http.Hijacker)
	return hj.Hijack()
}

func (bw *bufferedResponseWriter) Push(target string, opts *http.PushOptions) error {
	if pusher, ok := bw.ResponseWriter.(http.Pusher); ok {
		return pusher.Push(target, opts)
	}
	return http.ErrNotSupported
}

func (bw *bufferedResponseWriter) Flush() {
	// Take all from buf and flush
	bw.ResponseWriter.Write(bw.buf.Bytes())
	// Clear?? is this work, I need test more
	bw.buf.Reset()
	bw.ResponseWriter.(http.Flusher).Flush()
}

func (bw *bufferedResponseWriter) CloseNotify() <-chan bool {
	return bw.ResponseWriter.(http.CloseNotifier).CloseNotify()
}
