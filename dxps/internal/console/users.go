package console

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// Roles, lowest first: every role includes the ones before it.
var Roles = []string{"viewer", "operator", "configurator", "admin"}

func roleRank(r string) int { return slices.Index(Roles, r) }

// Allows reports whether one of have is at least need.
func Allows(have []string, need string) bool {
	n := roleRank(need)
	for _, r := range have {
		if roleRank(r) >= n && n >= 0 {
			return true
		}
	}
	return false
}

var (
	ErrBadLogin   = errors.New("invalid user name or password")
	ErrUserExists = errors.New("user already exists")
	ErrNoUser     = errors.New("user not found")
	ErrLastAdmin  = errors.New("at least one enabled admin must remain")
	reUser        = regexp.MustCompile(`^[a-z][a-z0-9._-]{1,31}$`)
)

const (
	pbkdfIter  = 600_000
	minPassLen = 12
)

type User struct {
	Name      string    `json:"name"`
	Roles     []string  `json:"roles"`
	Disabled  bool      `json:"disabled"`
	Hash      string    `json:"hash"` // pbkdf2-sha256$iter$salt$key
	Created   time.Time `json:"created"`
	LastLogin time.Time `json:"lastLogin,omitzero"`
}

// UserView is a user without the password hash.
type UserView struct {
	Name      string    `json:"name"`
	Roles     []string  `json:"roles"`
	Disabled  bool      `json:"disabled"`
	Created   time.Time `json:"created"`
	LastLogin time.Time `json:"lastLogin,omitzero"`
}

func (u *User) View() UserView {
	return UserView{Name: u.Name, Roles: u.Roles, Disabled: u.Disabled, Created: u.Created, LastLogin: u.LastLogin}
}

type Session struct {
	Token   string    `json:"-"`
	User    string    `json:"username"`
	Roles   []string  `json:"roles"`
	Expires time.Time `json:"expires_at"`
}

// Users is a file-backed user directory (mode 0600) with in-memory sessions.
type Users struct {
	path string
	ttl  time.Duration
	now  func() time.Time

	mu       sync.Mutex
	users    map[string]*User
	sessions map[string]*Session
	failures map[string]int
}

func OpenUsers(path string, ttl time.Duration) (*Users, error) {
	u := &Users{path: path, ttl: ttl, now: time.Now, users: map[string]*User{}, sessions: map[string]*Session{}, failures: map[string]int{}}
	b, err := os.ReadFile(path) // #nosec G304 -- operator runtime path
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		var list []*User
		if err := json.Unmarshal(b, &list); err != nil {
			return nil, fmt.Errorf("users file: %w", err)
		}
		for _, x := range list {
			u.users[x.Name] = x
		}
	}
	return u, nil
}

func (u *Users) save() error {
	list := make([]*User, 0, len(u.users))
	for _, x := range u.users {
		list = append(list, x)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	b, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(u.path), 0o700); err != nil {
		return err
	}
	tmp := u.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, u.path)
}

func hashPassword(pw string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	k, err := pbkdf2.Key(sha256.New, pw, salt, pbkdfIter, 32)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("pbkdf2-sha256$%d$%s$%s", pbkdfIter, base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(k)), nil
}

func checkPassword(hash, pw string) bool {
	parts := strings.Split(hash, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" {
		return false
	}
	var iter int
	if _, err := fmt.Sscanf(parts[1], "%d", &iter); err != nil || iter < 1 {
		return false
	}
	salt, err1 := base64.RawStdEncoding.DecodeString(parts[2])
	want, err2 := base64.RawStdEncoding.DecodeString(parts[3])
	if err1 != nil || err2 != nil {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, pw, salt, iter, len(want))
	return err == nil && subtle.ConstantTimeCompare(got, want) == 1
}

// dummyHash keeps the login time constant for unknown users.
var dummyHash, _ = hashPassword("dummy-password-for-timing")

func validRoles(roles []string) error {
	if len(roles) == 0 {
		return errors.New("at least one role is required")
	}
	for _, r := range roles {
		if roleRank(r) < 0 {
			return fmt.Errorf("unknown role %q", r)
		}
	}
	return nil
}

func validPassword(pw string) error {
	if len(pw) < minPassLen {
		return fmt.Errorf("password must have at least %d characters", minPassLen)
	}
	return nil
}

func (u *Users) Add(name, pw string, roles []string) error {
	if !reUser.MatchString(name) {
		return errors.New("user name: 2-32 characters, lower case letters, digits, . _ -")
	}
	if err := validRoles(roles); err != nil {
		return err
	}
	if err := validPassword(pw); err != nil {
		return err
	}
	h, err := hashPassword(pw)
	if err != nil {
		return err
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.users[name] != nil {
		return ErrUserExists
	}
	u.users[name] = &User{Name: name, Roles: roles, Hash: h, Created: u.now().UTC()}
	return u.save()
}

func (u *Users) enabledAdmins(except string) int {
	n := 0
	for _, x := range u.users {
		if x.Name != except && !x.Disabled && slices.Contains(x.Roles, "admin") {
			n++
		}
	}
	return n
}

// Update changes roles / disabled and optionally resets the password; sessions of the user are ended.
func (u *Users) Update(name string, roles []string, disabled bool, pw string) error {
	if err := validRoles(roles); err != nil {
		return err
	}
	var h string
	if pw != "" {
		if err := validPassword(pw); err != nil {
			return err
		}
		var err error
		if h, err = hashPassword(pw); err != nil {
			return err
		}
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	x := u.users[name]
	if x == nil {
		return ErrNoUser
	}
	if (disabled || !slices.Contains(roles, "admin")) && slices.Contains(x.Roles, "admin") && !x.Disabled && u.enabledAdmins(name) == 0 {
		return ErrLastAdmin
	}
	x.Roles, x.Disabled = roles, disabled
	if h != "" {
		x.Hash = h
	}
	u.endSessions(name)
	return u.save()
}

func (u *Users) Delete(name string) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	x := u.users[name]
	if x == nil {
		return ErrNoUser
	}
	if slices.Contains(x.Roles, "admin") && !x.Disabled && u.enabledAdmins(name) == 0 {
		return ErrLastAdmin
	}
	delete(u.users, name)
	u.endSessions(name)
	return u.save()
}

func (u *Users) List() []UserView {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := make([]UserView, 0, len(u.users))
	for _, x := range u.users {
		out = append(out, x.View())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (u *Users) Count() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.users)
}

// Login checks the password; five consecutive failures lock the account until an admin resets it.
func (u *Users) Login(name, pw string) (*Session, error) {
	u.mu.Lock()
	x := u.users[name]
	locked := u.failures[name] >= 5
	u.mu.Unlock()
	h := dummyHash
	if x != nil {
		h = x.Hash
	}
	ok := checkPassword(h, pw)
	u.mu.Lock()
	defer u.mu.Unlock()
	if x == nil || !ok || x.Disabled || locked {
		if x != nil && !ok {
			u.failures[name]++
		}
		return nil, ErrBadLogin
	}
	delete(u.failures, name)
	tok := make([]byte, 32)
	if _, err := rand.Read(tok); err != nil {
		return nil, err
	}
	s := &Session{Token: hex.EncodeToString(tok), User: name, Roles: x.Roles, Expires: u.now().Add(u.ttl).UTC()}
	u.sessions[s.Token] = s
	x.LastLogin = u.now().UTC()
	_ = u.save()
	return s, nil
}

func (u *Users) Session(token string) (*Session, bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	s := u.sessions[token]
	if s == nil {
		return nil, false
	}
	if u.now().After(s.Expires) {
		delete(u.sessions, token)
		return nil, false
	}
	return s, true
}

func (u *Users) Logout(token string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	delete(u.sessions, token)
}

func (u *Users) endSessions(name string) {
	for k, s := range u.sessions {
		if s.User == name {
			delete(u.sessions, k)
		}
	}
	delete(u.failures, name)
}

// ChangePassword lets a user change their own password (old password required).
func (u *Users) ChangePassword(name, old, pw string) error {
	if err := validPassword(pw); err != nil {
		return err
	}
	u.mu.Lock()
	x := u.users[name]
	u.mu.Unlock()
	if x == nil || !checkPassword(x.Hash, old) {
		return ErrBadLogin
	}
	h, err := hashPassword(pw)
	if err != nil {
		return err
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	x.Hash = h
	return u.save()
}
