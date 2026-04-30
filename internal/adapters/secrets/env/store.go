package env

import (
	"context"
	"errors"
	"fmt"
	"os"
)

var ErrUnsupported = errors.New("env secret store does not support mutation")

type Store struct{}

func New() *Store {
	return &Store{}
}

func (s *Store) Get(ctx context.Context, name string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if name == "" {
		return "", errors.New("secret name is empty")
	}
	value, ok := os.LookupEnv(name)
	if !ok {
		return "", fmt.Errorf("environment variable %s is not set", name)
	}
	return value, nil
}

func (s *Store) Set(ctx context.Context, name string, value string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return ErrUnsupported
}

func (s *Store) Delete(ctx context.Context, name string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return ErrUnsupported
}
