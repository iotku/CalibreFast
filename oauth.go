package main

// Identity is an external sign-in (GitHub, Google) linked to a user.
type Identity struct {
	Provider string
	Subject  string
	Email    string
	Login    string
}

func identitiesFor(userID int64) []Identity {
	rows, err := appDB.Query(`SELECT provider, subject, email, login FROM identities WHERE user_id = ? ORDER BY provider`, userID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []Identity
	for rows.Next() {
		var i Identity
		if rows.Scan(&i.Provider, &i.Subject, &i.Email, &i.Login) == nil {
			out = append(out, i)
		}
	}
	return out
}

// enabledProviders lists configured OAuth providers (none yet).
func enabledProviders() []string { return nil }

func configureOAuth() {}
