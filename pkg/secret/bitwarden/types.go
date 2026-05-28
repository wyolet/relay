package bitwarden

const (
	cipherTypeLogin      = 1
	cipherTypeSecureNote = 2
	cipherTypeCard       = 3
	cipherTypeIdentity   = 4
)

type preloginResponse struct {
	KDF            int  `json:"kdf"`
	KDFIterations  int  `json:"kdfIterations"`
	KDFMemory      *int `json:"kdfMemory"`
	KDFParallelism *int `json:"kdfParallelism"`
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	ExpiresIn    int    `json:"expires_in"`
	TokenType    string `json:"token_type"`
	RefreshToken string `json:"refresh_token"`
	Key          string `json:"Key"`
}

type syncResponse struct {
	Profile syncProfile  `json:"profile"`
	Ciphers []syncCipher `json:"ciphers"`
}

type syncProfile struct {
	ID            string             `json:"id"`
	Email         string             `json:"email"`
	Key           string             `json:"key"`
	PrivateKey    string             `json:"privateKey"`
	Organizations []syncOrganization `json:"organizations"`
}

type syncOrganization struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Key  string `json:"key"`
}

type syncCipher struct {
	ID             string      `json:"id"`
	Type           int         `json:"type"`
	OrganizationID *string     `json:"organizationId"`
	Name           string      `json:"name"`
	Notes          *string     `json:"notes"`
	Login          *syncLogin  `json:"login"`
	Fields         []syncField `json:"fields"`
}

type syncLogin struct {
	Username *string `json:"username"`
	Password *string `json:"password"`
	URI      *string `json:"uri"`
	URIs     []struct {
		URI *string `json:"uri"`
	} `json:"uris"`
}

type syncField struct {
	Name  *string `json:"name"`
	Value *string `json:"value"`
	Type  int     `json:"type"`
}

type decryptedItem struct {
	id       string
	name     string
	username string
	password string
	notes    string
	uri      string
	fields   map[string]string
}

type vaultCache struct {
	byExactName map[string][]decryptedItem
	byID        map[string]decryptedItem
}

func newVaultCache(items []decryptedItem) vaultCache {
	c := vaultCache{
		byExactName: make(map[string][]decryptedItem),
		byID:        make(map[string]decryptedItem, len(items)),
	}
	for _, item := range items {
		c.byID[item.id] = item
		c.byExactName[item.name] = append(c.byExactName[item.name], item)
	}
	return c
}
