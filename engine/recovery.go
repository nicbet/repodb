package engine

import (
	"fmt"

	"github.com/dolthub/go-mysql-server/sql"
	"github.com/dolthub/go-mysql-server/sql/types"
	"github.com/nicbet/repodb/common/repository"
)

type recoverCommitExpression struct {
	repo  *repository.Repository
	child sql.Expression
}

func (e *recoverCommitExpression) FunctionName() string { return "repodb_recover_commit" }
func (e *recoverCommitExpression) Description() string {
	return "resolves a RepoDB commit candidate as committed, rejected, or unknown"
}
func (e *recoverCommitExpression) Type() sql.Type   { return types.LongText }
func (e *recoverCommitExpression) IsNullable() bool { return false }
func (e *recoverCommitExpression) Resolved() bool   { return e.child.Resolved() }
func (e *recoverCommitExpression) String() string {
	return fmt.Sprintf("repodb_recover_commit(%s)", e.child)
}
func (e *recoverCommitExpression) Children() []sql.Expression { return []sql.Expression{e.child} }
func (e *recoverCommitExpression) CollationCoercibility(*sql.Context) (sql.CollationID, byte) {
	return sql.Collation_Default, 4
}
func (e *recoverCommitExpression) WithChildren(children ...sql.Expression) (sql.Expression, error) {
	if len(children) != 1 {
		return nil, sql.ErrInvalidChildrenNumber.New(e, len(children), 1)
	}
	return &recoverCommitExpression{repo: e.repo, child: children[0]}, nil
}
func (e *recoverCommitExpression) Eval(ctx *sql.Context, row sql.Row) (any, error) {
	value, err := e.child.Eval(ctx, row)
	if err != nil {
		return nil, err
	}
	candidate, ok := value.(string)
	if !ok {
		return nil, fmt.Errorf("repodb_recover_commit expects a commit ID string, got %T", value)
	}
	result, err := e.repo.RecoverCommit(ctx, candidate)
	if err != nil {
		return repository.OutcomeUnknown.String(), nil
	}
	return result.Outcome.String(), nil
}

var _ sql.Expression = (*recoverCommitExpression)(nil)
var _ sql.FunctionExpression = (*recoverCommitExpression)(nil)
var _ sql.CollationCoercible = (*recoverCommitExpression)(nil)
