package auth

import "time"

type Role string

const (
	RoleOwner  Role = "owner"
	RoleAdmin  Role = "admin"
	RoleMember Role = "member"
)

type Actor struct {
	UserID      string
	Username    string
	TeamID      string
	Role        Role
	DeviceID    string
	DeviceName  string
	SessionName string
	ClientType  string
}

type User struct {
	ID                 string
	Username           string
	UsernameNormalized string
	PasswordPHC        string
	PasswordVersion    int64
	Status             string
	MustChangePassword bool
	CreatedAt          time.Time
}

type Team struct {
	ID          string
	Name        string
	OwnerUserID string
	CreatedAt   time.Time
}

type Device struct {
	ID          string
	UserID      string
	Name        string
	SessionHash string
	CreatedAt   time.Time
	LastSeenAt  time.Time
}
