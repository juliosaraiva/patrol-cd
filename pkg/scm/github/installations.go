package github

import (
	"context"
	"errors"

	gh "github.com/google/go-github/v88/github"

	"github.com/juliosaraiva/patrol-cd/pkg/scm"
)

func asSCM(err error, target **scm.Error) bool { return errors.As(err, target) }

func toInstallation(i *gh.Installation) scm.Installation {
	return scm.Installation{
		ID:                  i.GetID(),
		Account:             i.GetAccount().GetLogin(),
		AccountType:         i.GetAccount().GetType(),
		RepositorySelection: i.GetRepositorySelection(),
		Suspended:           i.SuspendedAt != nil,
	}
}

// ListInstallations implements scm.InstallationLister (GET /app/installations with the
// App JWT, every page).
func (p *Provider) ListInstallations(ctx context.Context) ([]scm.Installation, error) {
	const op = "ListInstallations"
	if err := p.requireApp(op); err != nil {
		return nil, err
	}
	insts, err := collect(ctx, p, op, func(ctx context.Context, lo gh.ListOptions) ([]*gh.Installation, *gh.Response, error) {
		return p.app.client.Apps.ListInstallations(ctx, &lo)
	})
	if err != nil {
		return nil, err
	}
	out := make([]scm.Installation, 0, len(insts))
	for _, i := range insts {
		if i != nil {
			out = append(out, toInstallation(i))
		}
	}
	return out, nil
}

// FindInstallation implements scm.InstallationLister: GET /orgs/{owner}/installation,
// then GET /users/{owner}/installation. Results are cached for 60 minutes per
// (API URL, App ID, key, lowercase owner).
func (p *Provider) FindInstallation(ctx context.Context, owner string) (scm.Installation, error) {
	const op = "FindInstallation"
	if err := p.requireApp(op); err != nil {
		return scm.Installation{}, err
	}
	inst, err := p.app.find(ctx, owner)
	if err != nil {
		return scm.Installation{}, mapError(op, nil, err, p.now())
	}
	return inst, nil
}
