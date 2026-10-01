package coverletter

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"

	"coverletter/internal/apperror"
)

// Repository is the persistence port for cover letters.
//
// The interface is deliberately small: the service needs create, read-by-id,
// full update and a *conditional* status update. That last one is what makes
// the state machine safe under concurrency.
type Repository interface {
	Create(ctx context.Context, record *CoverLetter) error
	GetByID(ctx context.Context, id uint) (*CoverLetter, error)
	Update(ctx context.Context, record *CoverLetter) error

	// TransitionStatus moves a record from any status in `from` to `to`.
	// It returns ErrTransitionConflict when no row matched, which happens if
	// another worker advanced the record first.
	TransitionStatus(ctx context.Context, id uint, from []Status, to Status, errorMessage string) error
}

// ErrTransitionConflict reports that a status transition lost a race or was
// not legal from the record's current state.
var ErrTransitionConflict = errors.New("coverletter: illegal status transition")

// GORMRepository is the GORM-backed implementation.
type GORMRepository struct {
	db *gorm.DB
}

// NewRepository builds a GORM-backed repository.
func NewRepository(db *gorm.DB) *GORMRepository {
	return &GORMRepository{db: db}
}

// Create inserts a new record, populating its ID.
func (r *GORMRepository) Create(ctx context.Context, record *CoverLetter) error {
	if err := r.db.WithContext(ctx).Create(record).Error; err != nil {
		return apperror.Wrap(err, apperror.CodeInternal, "The request could not be saved.", 500)
	}
	return nil
}

// GetByID returns a record, or a not-found application error.
func (r *GORMRepository) GetByID(ctx context.Context, id uint) (*CoverLetter, error) {
	var record CoverLetter
	err := r.db.WithContext(ctx).First(&record, id).Error
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		return nil, apperror.NotFound("That cover letter does not exist.")
	case err != nil:
		return nil, apperror.Wrap(err, apperror.CodeInternal, "The cover letter could not be loaded.", 500)
	}
	return &record, nil
}

// Update writes every mutable field of a record.
func (r *GORMRepository) Update(ctx context.Context, record *CoverLetter) error {
	err := r.db.WithContext(ctx).Model(record).Select(
		"job_url",
		"job_description",
		"resume_path",
		"resume_filename",
		"resume_text",
		"cover_letter_text",
		"candidate_name",
		"pdf_path",
		"status",
		"error_message",
		"validation",
	).Updates(record).Error
	if err != nil {
		return apperror.Wrap(err, apperror.CodeInternal, "The cover letter could not be updated.", 500)
	}
	return nil
}

// TransitionStatus implements Repository.
//
// The WHERE clause includes the expected source statuses, so the update is a
// compare-and-swap: exactly one caller can move a given record out of a given
// state. That is what prevents a duplicate worker or a retry from stomping on a
// completed record.
func (r *GORMRepository) TransitionStatus(ctx context.Context, id uint, from []Status, to Status, errorMessage string) error {
	if !to.IsValid() {
		return apperror.Internal(fmt.Errorf("coverletter: unknown target status %q", to), "The status update was invalid.")
	}

	for _, candidate := range from {
		if !CanTransition(candidate, to) {
			return fmt.Errorf("%w: %s -> %s", ErrTransitionConflict, candidate, to)
		}
	}

	updates := map[string]any{
		"status":        to,
		"error_message": errorMessage,
		"updated_at":    time.Now().UTC(),
	}

	result := r.db.WithContext(ctx).
		Model(&CoverLetter{}).
		Where("id = ? AND status IN ?", id, from).
		Updates(updates)
	if result.Error != nil {
		return apperror.Wrap(result.Error, apperror.CodeInternal, "The status could not be updated.", 500)
	}
	if result.RowsAffected == 0 {
		return fmt.Errorf("%w: record %d is no longer in %v", ErrTransitionConflict, id, from)
	}
	return nil
}
