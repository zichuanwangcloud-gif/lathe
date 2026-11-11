package linear

import (
	"context"
	"errors"
)

// CreateIssueInput 是 issueCreate 的输入。
type CreateIssueInput struct {
	ID          string
	TeamID      string
	Title       string
	Description string
	Priority    int
	AssigneeID  string
	LabelIDs    []string
}

// CreatedIssue 是建单/查单返回的三个字段。
type CreatedIssue struct {
	ID         string `json:"id"`
	Identifier string `json:"identifier"`
	URL        string `json:"url"`
}

// Team 是 Linear 团队。
type Team struct {
	ID   string `json:"id"`
	Key  string `json:"key"`
	Name string `json:"name"`
}

var errNotImplemented = errors.New("linear: 未实现")

// CreateIssue 桩。
func (c *Client) CreateIssue(ctx context.Context, in CreateIssueInput) (*CreatedIssue, error) {
	return nil, errNotImplemented
}

// Teams 桩。
func (c *Client) Teams(ctx context.Context) ([]Team, error) { return nil, errNotImplemented }

// LabelIDByName 桩。
func (c *Client) LabelIDByName(ctx context.Context, teamID, name string) (string, error) {
	return "", errNotImplemented
}

// IssueByID 桩。
func (c *Client) IssueByID(ctx context.Context, id string) (*CreatedIssue, error) {
	return nil, errNotImplemented
}
