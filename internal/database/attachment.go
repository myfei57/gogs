package database

import (
	"fmt"
	"io"
	"mime/multipart"
	"os"
	"path"
	"sort"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/google/uuid"
	"xorm.io/xorm"

	"gogs.io/gogs/internal/conf"
	"gogs.io/gogs/internal/errx"
)

// Attachment represent a attachment of issue/comment/release.
type Attachment struct {
	ID        int64
	UUID      string `xorm:"uuid UNIQUE"`
	IssueID   int64  `xorm:"INDEX"`
	CommentID int64
	ReleaseID int64 `xorm:"INDEX"`
	Name      string

	Created     time.Time `xorm:"-" json:"-" gorm:"-"`
	CreatedUnix int64
}

func (a *Attachment) BeforeInsert() {
	a.CreatedUnix = time.Now().Unix()
}

func (a *Attachment) AfterSet(colName string, _ xorm.Cell) {
	switch colName {
	case "created_unix":
		a.Created = time.Unix(a.CreatedUnix, 0).Local()
	}
}

// AttachmentLocalPath returns where attachment is stored in local file system based on given UUID.
func AttachmentLocalPath(uuid string) string {
	return path.Join(conf.Attachment.Path, uuid[0:1], uuid[1:2], uuid)
}

// LocalPath returns where attachment is stored in local file system.
func (a *Attachment) LocalPath() string {
	return AttachmentLocalPath(a.UUID)
}

// NewAttachment creates a new attachment object.
func NewAttachment(name string, buf []byte, file multipart.File) (_ *Attachment, err error) {
	attach := &Attachment{
		UUID: uuid.New().String(),
		Name: name,
	}

	localPath := attach.LocalPath()
	if err = os.MkdirAll(path.Dir(localPath), os.ModePerm); err != nil {
		return nil, errors.Newf("MkdirAll: %v", err)
	}

	fw, err := os.Create(localPath)
	if err != nil {
		return nil, errors.Newf("Create: %v", err)
	}
	defer fw.Close()

	if _, err = fw.Write(buf); err != nil {
		return nil, errors.Newf("write: %v", err)
	} else if _, err = io.Copy(fw, file); err != nil {
		return nil, errors.Newf("copy: %v", err)
	}

	if _, err := x.Insert(attach); err != nil {
		return nil, err
	}

	return attach, nil
}

var _ errx.NotFound = (*ErrAttachmentNotExist)(nil)

type ErrAttachmentNotExist struct {
	args map[string]any
}

func IsErrAttachmentNotExist(err error) bool {
	_, ok := err.(ErrAttachmentNotExist)
	return ok
}

func (err ErrAttachmentNotExist) Error() string {
	return fmt.Sprintf("attachment does not exist: %v", err.args)
}

func (ErrAttachmentNotExist) NotFound() bool {
	return true
}

func getAttachmentByUUID(e Engine, uuid string) (*Attachment, error) {
	attach := &Attachment{UUID: uuid}
	has, err := e.Get(attach)
	if err != nil {
		return nil, err
	} else if !has {
		return nil, ErrAttachmentNotExist{args: map[string]any{"uuid": uuid}}
	}
	return attach, nil
}

func getAttachmentsByUUIDs(e Engine, uuids []string) ([]*Attachment, error) {
	if len(uuids) == 0 {
		return []*Attachment{}, nil
	}

	// Silently drop invalid uuids.
	attachments := make([]*Attachment, 0, len(uuids))
	return attachments, e.In("uuid", uuids).Find(&attachments)
}

// GetAttachmentByUUID returns attachment by given UUID.
func GetAttachmentByUUID(uuid string) (*Attachment, error) {
	return getAttachmentByUUID(x, uuid)
}

func getAttachmentsByIssueID(e Engine, issueID int64) ([]*Attachment, error) {
	attachments := make([]*Attachment, 0, 5)
	return attachments, e.Where("issue_id = ? AND comment_id = 0", issueID).Find(&attachments)
}

// GetAttachmentsByIssueID returns all attachments of an issue.
func GetAttachmentsByIssueID(issueID int64) ([]*Attachment, error) {
	return getAttachmentsByIssueID(x, issueID)
}

func getAttachmentsByCommentID(e Engine, commentID int64) ([]*Attachment, error) {
	attachments := make([]*Attachment, 0, 5)
	return attachments, e.Where("comment_id=?", commentID).Find(&attachments)
}

// GetAttachmentsByCommentID returns all attachments of a comment.
func GetAttachmentsByCommentID(commentID int64) ([]*Attachment, error) {
	return getAttachmentsByCommentID(x, commentID)
}

func getAttachmentsByReleaseID(e Engine, releaseID int64) ([]*Attachment, error) {
	attachments := make([]*Attachment, 0, 10)
	return attachments, e.Where("release_id = ?", releaseID).Find(&attachments)
}

// GetAttachmentsByReleaseID returns all attachments of a release.
func GetAttachmentsByReleaseID(releaseID int64) ([]*Attachment, error) {
	return getAttachmentsByReleaseID(x, releaseID)
}

// DuplicateAttachment represents a group of release attachments that share
// the same original name.
type DuplicateAttachment struct {
	Name          string  `json:"name"`
	AttachmentIDs []int64 `json:"attachment_ids"`
}

// duplicateAttachments groups attachments by their original name and keeps
// groups that contain at least two attachments. The grouping is case
// sensitive, and attachments whose name is empty are grouped separately.
// The result is sorted by name, and the IDs within each group are sorted in
// ascending order.
func duplicateAttachments(attachments []*Attachment) []DuplicateAttachment {
	groups := make(map[string][]int64, len(attachments))
	for _, attach := range attachments {
		groups[attach.Name] = append(groups[attach.Name], attach.ID)
	}

	duplicates := make([]DuplicateAttachment, 0, len(groups))
	for name, ids := range groups {
		if len(ids) < 2 {
			continue
		}
		sort.SliceStable(ids, func(i, j int) bool { return ids[i] < ids[j] })
		duplicates = append(duplicates, DuplicateAttachment{
			Name:          name,
			AttachmentIDs: ids,
		})
	}
	sort.SliceStable(duplicates, func(i, j int) bool {
		return duplicates[i].Name < duplicates[j].Name
	})
	return duplicates
}

func getDuplicateAttachmentsByReleaseID(e Engine, releaseID int64) ([]DuplicateAttachment, error) {
	// Only the metadata columns needed for the comparison are queried.
	// Attachment contents are stored on disk and are never read.
	attachments := make([]*Attachment, 0, 10)
	if err := e.Where("release_id = ?", releaseID).
		Cols("id", "name").
		Find(&attachments); err != nil {
		return nil, err
	}
	return duplicateAttachments(attachments), nil
}

// GetDuplicateAttachmentsByReleaseID returns original names that are used by
// two or more attachments of the release, along with the attachment IDs of
// each group. It reads attachment metadata only and never modifies or
// deletes attachments.
func GetDuplicateAttachmentsByReleaseID(releaseID int64) ([]DuplicateAttachment, error) {
	return getDuplicateAttachmentsByReleaseID(x, releaseID)
}

// DeleteAttachment deletes the given attachment and optionally the associated file.
func DeleteAttachment(a *Attachment, remove bool) error {
	_, err := DeleteAttachments([]*Attachment{a}, remove)
	return err
}

// DeleteAttachments deletes the given attachments and optionally the associated files.
func DeleteAttachments(attachments []*Attachment, remove bool) (int, error) {
	for i, a := range attachments {
		if remove {
			if err := os.Remove(a.LocalPath()); err != nil {
				return i, err
			}
		}

		if _, err := x.Delete(a); err != nil {
			return i, err
		}
	}

	return len(attachments), nil
}

// DeleteAttachmentsByIssue deletes all attachments associated with the given issue.
func DeleteAttachmentsByIssue(issueID int64, remove bool) (int, error) {
	attachments, err := GetAttachmentsByIssueID(issueID)
	if err != nil {
		return 0, err
	}

	return DeleteAttachments(attachments, remove)
}

// DeleteAttachmentsByComment deletes all attachments associated with the given comment.
func DeleteAttachmentsByComment(commentID int64, remove bool) (int, error) {
	attachments, err := GetAttachmentsByCommentID(commentID)
	if err != nil {
		return 0, err
	}

	return DeleteAttachments(attachments, remove)
}
