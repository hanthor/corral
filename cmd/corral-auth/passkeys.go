package main

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	wa "github.com/go-webauthn/webauthn/webauthn"
)

type passkeyUser struct {
	ID          []byte          `json:"id"`
	Name        string          `json:"name"`
	Credentials []wa.Credential `json:"credentials"`
}

func (u *passkeyUser) WebAuthnID() []byte                   { return u.ID }
func (u *passkeyUser) WebAuthnName() string                 { return u.Name }
func (u *passkeyUser) WebAuthnDisplayName() string          { return u.Name }
func (u *passkeyUser) WebAuthnCredentials() []wa.Credential { return u.Credentials }

type passkeyCeremony struct {
	Kind    string
	User    string
	Data    wa.SessionData
	Expires time.Time
}
type passkeyManager struct {
	mu          sync.Mutex
	path        string
	enrollToken string
	web         *wa.WebAuthn
	users       map[string]*passkeyUser
	ceremonies  map[string]passkeyCeremony
}

func newPasskeyManager(path, rpID, origin, enrollToken string) (*passkeyManager, error) {
	if rpID == "" || origin == "" {
		return nil, fmt.Errorf("passkeys require --passkey-rp-id and --passkey-origin")
	}
	w, err := wa.New(&wa.Config{RPID: rpID, RPDisplayName: "Corral", RPOrigins: []string{origin}, AuthenticatorSelection: protocol.AuthenticatorSelection{ResidentKey: protocol.ResidentKeyRequirementRequired, UserVerification: protocol.VerificationRequired}})
	if err != nil {
		return nil, fmt.Errorf("passkey configuration: %w", err)
	}
	p := &passkeyManager{path: path, enrollToken: enrollToken, web: w, users: map[string]*passkeyUser{}, ceremonies: map[string]passkeyCeremony{}}
	if data, err := os.ReadFile(path); err == nil {
		var users []*passkeyUser
		if err := json.Unmarshal(data, &users); err != nil {
			return nil, fmt.Errorf("passkey store: %w", err)
		}
		for _, user := range users {
			p.users[user.Name] = user
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	return p, nil
}

func (p *passkeyManager) saveLocked() error {
	users := make([]*passkeyUser, 0, len(p.users))
	for _, user := range p.users {
		users = append(users, user)
	}
	data, err := json.MarshalIndent(users, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(p.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".passkeys-*.tmp")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(name, p.path)
}
func randomID(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b
}
func (p *passkeyManager) ceremony(kind, user string, data *wa.SessionData) string {
	id := base64.RawURLEncoding.EncodeToString(randomID(24))
	p.ceremonies[id] = passkeyCeremony{Kind: kind, User: user, Data: *data, Expires: time.Now().Add(5 * time.Minute)}
	return id
}
func (p *passkeyManager) take(id, kind string) (passkeyCeremony, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	c, ok := p.ceremonies[id]
	delete(p.ceremonies, id)
	if !ok || c.Kind != kind || time.Now().After(c.Expires) {
		return passkeyCeremony{}, fmt.Errorf("passkey ceremony expired or invalid")
	}
	return c, nil
}

func (g *gateway) registerPasskeyRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /auth/passkey", g.passkeyPage)
	m.HandleFunc("POST /auth/passkey/login/begin", g.passkeyLoginBegin)
	m.HandleFunc("POST /auth/passkey/login/finish", g.passkeyLoginFinish)
	m.HandleFunc("POST /auth/passkey/enroll/begin", g.passkeyEnrollBegin)
	m.HandleFunc("POST /auth/passkey/enroll/finish", g.passkeyEnrollFinish)
}
func passkeyJSON(w http.ResponseWriter, code int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(value)
}
func (g *gateway) passkeyLoginBegin(w http.ResponseWriter, r *http.Request) {
	assertion, session, err := g.passkeys.web.BeginDiscoverableLogin(wa.WithUserVerification(protocol.VerificationRequired))
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	g.passkeys.mu.Lock()
	id := g.passkeys.ceremony("login", "", session)
	g.passkeys.mu.Unlock()
	passkeyJSON(w, 200, map[string]any{"ceremony": id, "publicKey": assertion.Response})
}
func (g *gateway) passkeyLoginFinish(w http.ResponseWriter, r *http.Request) {
	c, err := g.passkeys.take(r.URL.Query().Get("ceremony"), "login")
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	var matched *passkeyUser
	handler := func(rawID, userHandle []byte) (wa.User, error) {
		g.passkeys.mu.Lock()
		defer g.passkeys.mu.Unlock()
		for _, user := range g.passkeys.users {
			if subtle.ConstantTimeCompare(user.ID, userHandle) == 1 {
				matched = user
				return user, nil
			}
			for _, cred := range user.Credentials {
				if subtle.ConstantTimeCompare(cred.ID, rawID) == 1 {
					matched = user
					return user, nil
				}
			}
		}
		return nil, fmt.Errorf("unknown passkey")
	}
	user, credential, err := g.passkeys.web.FinishPasskeyLogin(handler, c.Data, r)
	if err != nil {
		http.Error(w, "passkey verification failed", 401)
		return
	}
	if matched == nil {
		matched, _ = user.(*passkeyUser)
	}
	if matched == nil {
		http.Error(w, "passkey user missing", 500)
		return
	}
	g.passkeys.mu.Lock()
	for i := range matched.Credentials {
		if subtle.ConstantTimeCompare(matched.Credentials[i].ID, credential.ID) == 1 {
			matched.Credentials[i] = *credential
		}
	}
	err = g.passkeys.saveLocked()
	g.passkeys.mu.Unlock()
	if err != nil {
		http.Error(w, "saving passkey", 500)
		return
	}
	s, _ := g.session(r)
	s.Values = map[interface{}]interface{}{"login": "passkey|" + base64.RawURLEncoding.EncodeToString(matched.ID), "name": matched.Name}
	if err := s.Save(r, w); err != nil {
		http.Error(w, "saving session", 500)
		return
	}
	passkeyJSON(w, 200, map[string]bool{"ok": true})
}
func (g *gateway) enrollmentIdentity(r *http.Request) (string, bool) {
	if login, name, _ := g.identity(r); login != "" {
		if name == "" {
			name = login
		}
		return name, true
	}
	provided, user := r.Header.Get("X-Corral-Enroll-Token"), r.URL.Query().Get("user")
	expected := g.passkeys.enrollToken
	return user, user != "" && expected != "" && len(provided) == len(expected) && subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) == 1
}
func (g *gateway) passkeyEnrollBegin(w http.ResponseWriter, r *http.Request) {
	name, ok := g.enrollmentIdentity(r)
	if !ok {
		http.Error(w, "authenticated session or enrollment token required", 401)
		return
	}
	g.passkeys.mu.Lock()
	user := g.passkeys.users[name]
	if user == nil {
		user = &passkeyUser{ID: randomID(32), Name: name}
		g.passkeys.users[name] = user
		if err := g.passkeys.saveLocked(); err != nil {
			g.passkeys.mu.Unlock()
			http.Error(w, err.Error(), 500)
			return
		}
	}
	creation, session, err := g.passkeys.web.BeginRegistration(user, wa.WithResidentKeyRequirement(protocol.ResidentKeyRequirementRequired), wa.WithExclusions(wa.Credentials(user.Credentials).CredentialDescriptors()))
	if err != nil {
		g.passkeys.mu.Unlock()
		http.Error(w, err.Error(), 500)
		return
	}
	id := g.passkeys.ceremony("enroll", name, session)
	g.passkeys.mu.Unlock()
	passkeyJSON(w, 200, map[string]any{"ceremony": id, "publicKey": creation.Response})
}
func (g *gateway) passkeyEnrollFinish(w http.ResponseWriter, r *http.Request) {
	c, err := g.passkeys.take(r.URL.Query().Get("ceremony"), "enroll")
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	g.passkeys.mu.Lock()
	user := g.passkeys.users[c.User]
	g.passkeys.mu.Unlock()
	if user == nil {
		http.Error(w, "enrollment user missing", 400)
		return
	}
	credential, err := g.passkeys.web.FinishRegistration(user, c.Data, r)
	if err != nil {
		http.Error(w, "passkey registration failed", 400)
		return
	}
	g.passkeys.mu.Lock()
	user.Credentials = append(user.Credentials, *credential)
	err = g.passkeys.saveLocked()
	g.passkeys.mu.Unlock()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	passkeyJSON(w, 200, map[string]bool{"ok": true})
}

func (g *gateway) passkeyPage(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(passkeyHTML))
}

const passkeyHTML = `<!doctype html><meta charset="utf-8"><meta name="viewport" content="width=device-width"><title>Corral passkey</title>
<style>body{font:16px system-ui;max-width:34rem;margin:10vh auto;padding:2rem;background:#111;color:#eee}button,input{font:inherit;padding:.7rem;margin:.3rem}button{background:#e65bb6;border:0;border-radius:.4rem}#status{white-space:pre-wrap;color:#bbb}</style>
<h1>Corral</h1><p>Sign in with a passkey, or enroll one using an authenticated Basic/OIDC session or administrator bootstrap token.</p>
<button id="login">Sign in with passkey</button><hr><input id="user" placeholder="Enrollment name"><input id="token" type="password" placeholder="Bootstrap token (optional)"><button id="enroll">Enroll passkey</button><p id="status"></p>
<script>
const s=document.querySelector('#status'), b64=s=>Uint8Array.from(atob(s.replace(/-/g,'+').replace(/_/g,'/')+'='.repeat((4-s.length%4)%4)),c=>c.charCodeAt(0)), enc=b=>btoa(String.fromCharCode(...new Uint8Array(b))).replace(/\+/g,'-').replace(/\//g,'_').replace(/=+$/,'');
function decode(o){o.challenge=b64(o.challenge);if(o.user)o.user.id=b64(o.user.id);for(const k of ['allowCredentials','excludeCredentials'])if(o[k])o[k].forEach(c=>c.id=b64(c.id));return o}
function pack(c){const r=c.response,x={id:c.id,rawId:enc(c.rawId),type:c.type,response:{clientDataJSON:enc(r.clientDataJSON)},clientExtensionResults:c.getClientExtensionResults()};for(const k of ['attestationObject','authenticatorData','signature','userHandle'])if(r[k])x.response[k]=enc(r[k]);return x}
async function flow(kind){s.textContent='Waiting for authenticator…';const enroll=kind==='enroll', user=encodeURIComponent(document.querySelector('#user').value), token=document.querySelector('#token').value;const h=token?{'X-Corral-Enroll-Token':token}:{};const q=enroll?'?user='+user:'';const start=await fetch('/auth/passkey/'+kind+'/begin'+q,{method:'POST',headers:h});if(!start.ok)throw Error(await start.text());const o=await start.json();const cred=enroll?await navigator.credentials.create({publicKey:decode(o.publicKey)}):await navigator.credentials.get({publicKey:decode(o.publicKey)});const done=await fetch('/auth/passkey/'+kind+'/finish?ceremony='+encodeURIComponent(o.ceremony),{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(pack(cred))});if(!done.ok)throw Error(await done.text());s.textContent=enroll?'Passkey enrolled. You can sign in now.':'Signed in. Redirecting…';if(!enroll)location.href='/'}
document.querySelector('#login').onclick=()=>flow('login').catch(e=>s.textContent=e);document.querySelector('#enroll').onclick=()=>flow('enroll').catch(e=>s.textContent=e);
</script>`
