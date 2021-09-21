package v1

import "fmt"

type TokenError struct {
	Msg string
}

func (e *TokenError) Error() string {
	return fmt.Sprintf("kubetest: %s", e.Msg)
}

func errInvalidTokenName(name string) error {
	return &TokenError{Msg: fmt.Sprintf("specified undefined token name %s", name)}
}

type RepositoryError struct {
	Msg string
}

func (e *RepositoryError) Error() string {
	return fmt.Sprintf("kubetest: %s", e.Msg)
}

func errInvalidRepoName(name string) error {
	return &RepositoryError{Msg: fmt.Sprintf("%s is undefined repository name", name)}
}

type ArtifactError struct {
	Msg string
}

func (e *ArtifactError) Error() string {
	return fmt.Sprintf("kubetest: %s", e.Msg)
}

func errInvalidArtifactName(name string) error {
	return &ArtifactError{Msg: fmt.Sprintf("specified undefined artifact name %s", name)}
}
