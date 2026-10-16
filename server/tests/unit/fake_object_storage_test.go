package unit

import (
	"context"
	"fmt"
	"time"

	"github.com/sanjeev0303/object-storage-platform/internal/interfaces"
)

type fakeObjectStorage struct{}

func (f *fakeObjectStorage) CreateUploadPlan(
	_ context.Context,
	target interfaces.UploadTarget,
) (interfaces.UploadPlan, error) {
	return interfaces.UploadPlan{
		UploadURL: fmt.Sprintf(
			"https://storage.example.test/%s",
			target.ObjectKey,
		),
		ExpiresAt: time.Now().UTC().Add(15 * time.Minute),
	}, nil
}

func (f *fakeObjectStorage) HeadObject(
	context.Context,
	string,
	string,
) (interfaces.ObjectMetadata, error) {
	return interfaces.ObjectMetadata{}, nil
}

func (f *fakeObjectStorage) DeleteObject(
	context.Context,
	string,
	string,
) error {
	return nil
}
