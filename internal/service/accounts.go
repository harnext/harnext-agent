package service

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

const (
	// MaxAccounts is how many Harnext accounts one machine can hold (a person with a personal and a company account needs two).
	MaxAccounts      = 8
	maxAccountsSize  = 16 * 1024
	accountsFileVers = 1
)

// ErrNoSuchAccount is returned when RemoveAccount finds no account for the given id.
var ErrNoSuchAccount = errors.New("service: no such account")

// ErrTooManyAccounts is returned when a ninth account is added.
var ErrTooManyAccounts = errors.New("service: too many accounts on this machine")

// ErrAmbiguousAccount is returned when an id prefix matches more than one account.
var ErrAmbiguousAccount = errors.New("service: more than one account matches")

// Account is one Harnext account on this machine: the agent token it issued. ID is a short local label derived from the token
// (it is not a secret and never leaves the machine); it names the account in status output and routes uploads.
type Account struct {
	ID    string
	Token string
}

// AccountID derives the local label of a token.
func AccountID(token string) string {
	sum := sha256.Sum256([]byte("harnext:account-id:v1:" + token))
	return hex.EncodeToString(sum[:])[:10]
}

type accountsFile struct {
	Version  int `json:"v"`
	Accounts []struct {
		Token string `json:"token"`
	} `json:"accounts"`
}

// IdentityFileFor is the stored identity document of one account.
func (p Paths) IdentityFileFor(id string) string {
	return filepath.Join(p.Dir, "agent-identity-"+id+".json")
}

// LoadAccounts reads the accounts in the order they were added; ErrNotInitialized when there is none.
func (p Paths) LoadAccounts() ([]Account, error) {
	data, err := readLimited(p.Accounts, maxAccountsSize)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotInitialized
	}
	if err != nil {
		return nil, errors.New("service: read accounts")
	}
	var f accountsFile
	if json.Unmarshal(data, &f) != nil || f.Version != accountsFileVers || len(f.Accounts) > MaxAccounts {
		return nil, errors.New("service: accounts file is invalid")
	}
	out := make([]Account, 0, len(f.Accounts))
	seen := map[string]bool{}
	for _, a := range f.Accounts {
		tok := strings.TrimSpace(a.Token)
		if ValidateToken(tok) != nil || seen[tok] {
			return nil, errors.New("service: accounts file is invalid")
		}
		seen[tok] = true
		out = append(out, Account{ID: AccountID(tok), Token: tok})
	}
	if len(out) == 0 {
		return nil, ErrNotInitialized
	}
	return out, nil
}

func (p Paths) saveAccounts(list []Account) error {
	f := accountsFile{Version: accountsFileVers}
	for _, a := range list {
		f.Accounts = append(f.Accounts, struct {
			Token string `json:"token"`
		}{a.Token})
	}
	data, err := json.Marshal(f)
	if err != nil {
		return errors.New("service: encode accounts")
	}
	return writeAtomic(p.Dir, p.Accounts, data)
}

// AddAccount validates the token and adds its account (0600 file). Adding a token that is already stored changes nothing.
func (p Paths) AddAccount(tok string) (Account, error) {
	if err := ValidateToken(tok); err != nil {
		return Account{}, err
	}
	list, err := p.LoadAccounts()
	if err != nil && !errors.Is(err, ErrNotInitialized) {
		return Account{}, err
	}
	for _, a := range list {
		if a.Token == tok {
			return a, nil
		}
	}
	if len(list) >= MaxAccounts {
		return Account{}, ErrTooManyAccounts
	}
	acct := Account{ID: AccountID(tok), Token: tok}
	return acct, p.saveAccounts(append(list, acct))
}

// SaveToken adds the account of this token (see AddAccount).
func (p Paths) SaveToken(tok string) error {
	_, err := p.AddAccount(tok)
	return err
}

// RemoveAccount removes the account whose id starts with idPrefix (an empty prefix means the only account) and deletes its
// stored identity document. It returns the removed account.
func (p Paths) RemoveAccount(idPrefix string) (Account, error) {
	list, err := p.LoadAccounts()
	if err != nil {
		return Account{}, err
	}
	var hit []int
	for i, a := range list {
		if strings.HasPrefix(a.ID, idPrefix) {
			hit = append(hit, i)
		}
	}
	switch {
	case len(hit) == 0:
		return Account{}, ErrNoSuchAccount
	case len(hit) > 1:
		return Account{}, ErrAmbiguousAccount
	}
	gone := list[hit[0]]
	rest := append(append([]Account{}, list[:hit[0]]...), list[hit[0]+1:]...)
	if len(rest) == 0 {
		if err := os.Remove(p.Accounts); err != nil && !errors.Is(err, os.ErrNotExist) {
			return Account{}, errors.New("service: write file")
		}
	} else if err := p.saveAccounts(rest); err != nil {
		return Account{}, err
	}
	_ = os.Remove(p.IdentityFileFor(gone.ID))
	return gone, nil
}
