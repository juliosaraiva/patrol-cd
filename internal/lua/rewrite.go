package lua

import (
	"github.com/yuin/gopher-lua/ast"
)

// The .. operator compiles to OP_CONCAT, which gopher-lua executes without any hook: a
// script doubling a string reaches gigabytes within its deadline. compile therefore
// rewrites every concatenation into a call of a guarded Go function (allocBudget.concat)
// held in a hidden local of the main chunk:
//
//	local <concat> = ...      -- the evaluation passes the function as the chunk argument
//	x = a .. b .. c           -- becomes x = (<concat>(a, b, c))
//
// Right-nested chains are flattened exactly as gopher-lua's compiler builds one
// OP_CONCAT, and the Go function reproduces OP_CONCAT's semantics (operand order,
// __concat metamethods, number formatting and error text). Errors keep their line: the
// call carries the concatenation's position.
//
// The local's name is not a valid identifier, so scripts cannot name it. Varargs of the
// main chunk ('...' outside any function) are rewritten into a call of the same function
// with no arguments, which returns nothing, so scripts still see an empty '...'.

// concatLocal names the hidden local.
const concatLocal = "(patrol concat)"

// rewriteChunk applies the rewrite to a parsed main chunk in place and returns the new
// statement list.
func rewriteChunk(stmts []ast.Stmt) []ast.Stmt {
	rewriteStmts(stmts, true)
	decl := &ast.LocalAssignStmt{Names: []string{concatLocal}, Exprs: []ast.Expr{&ast.Comma3Expr{}}}
	return append([]ast.Stmt{decl}, stmts...)
}

func concatCall(line, lastLine int, args []ast.Expr) *ast.FuncCallExpr {
	fn := &ast.IdentExpr{Value: concatLocal}
	fn.SetLine(line)
	fn.SetLastLine(lastLine)
	// AdjustRet: a single result and never a tail call, like the operator.
	call := &ast.FuncCallExpr{Func: fn, Args: args, AdjustRet: true}
	call.SetLine(line)
	call.SetLastLine(lastLine)
	return call
}

// rewriteStmts rewrites statements in place. main is true for statements of the main
// chunk's own body (outside nested functions).
func rewriteStmts(stmts []ast.Stmt, main bool) {
	for _, s := range stmts {
		rewriteStmt(s, main)
	}
}

func rewriteExprs(exprs []ast.Expr, main bool) {
	for i := range exprs {
		exprs[i] = rewriteExpr(exprs[i], main)
	}
}

func rewriteStmt(s ast.Stmt, main bool) {
	switch st := s.(type) {
	case *ast.AssignStmt:
		rewriteExprs(st.Lhs, main)
		rewriteExprs(st.Rhs, main)
	case *ast.LocalAssignStmt:
		rewriteExprs(st.Exprs, main)
	case *ast.FuncCallStmt:
		st.Expr = rewriteExpr(st.Expr, main)
	case *ast.DoBlockStmt:
		rewriteStmts(st.Stmts, main)
	case *ast.WhileStmt:
		st.Condition = rewriteExpr(st.Condition, main)
		rewriteStmts(st.Stmts, main)
	case *ast.RepeatStmt:
		st.Condition = rewriteExpr(st.Condition, main)
		rewriteStmts(st.Stmts, main)
	case *ast.IfStmt:
		st.Condition = rewriteExpr(st.Condition, main)
		rewriteStmts(st.Then, main)
		rewriteStmts(st.Else, main)
	case *ast.NumberForStmt:
		st.Init = rewriteExpr(st.Init, main)
		st.Limit = rewriteExpr(st.Limit, main)
		st.Step = rewriteExpr(st.Step, main)
		rewriteStmts(st.Stmts, main)
	case *ast.GenericForStmt:
		rewriteExprs(st.Exprs, main)
		rewriteStmts(st.Stmts, main)
	case *ast.FuncDefStmt:
		if st.Name != nil {
			st.Name.Func = rewriteExpr(st.Name.Func, main)
			st.Name.Receiver = rewriteExpr(st.Name.Receiver, main)
		}
		if st.Func != nil {
			rewriteStmts(st.Func.Stmts, false)
		}
	case *ast.ReturnStmt:
		rewriteExprs(st.Exprs, main)
	}
}

func rewriteExpr(e ast.Expr, main bool) ast.Expr {
	switch ex := e.(type) {
	case *ast.StringConcatOpExpr:
		// a .. (b .. (c .. d)) is one OP_CONCAT over a, b, c, d; a left operand that is
		// itself a concatenation is evaluated first, as a separate operation.
		args := []ast.Expr{rewriteExpr(ex.Lhs, main)}
		rest := ex.Rhs
		for {
			next, ok := rest.(*ast.StringConcatOpExpr)
			if !ok {
				break
			}
			args = append(args, rewriteExpr(next.Lhs, main))
			rest = next.Rhs
		}
		args = append(args, rewriteExpr(rest, main))
		return concatCall(ex.Line(), ex.LastLine(), args)
	case *ast.Comma3Expr:
		if !main {
			return ex
		}
		call := concatCall(ex.Line(), ex.LastLine(), nil)
		call.AdjustRet = ex.AdjustRet
		return call
	case *ast.AttrGetExpr:
		ex.Object = rewriteExpr(ex.Object, main)
		ex.Key = rewriteExpr(ex.Key, main)
	case *ast.TableExpr:
		for _, f := range ex.Fields {
			f.Key = rewriteExpr(f.Key, main)
			f.Value = rewriteExpr(f.Value, main)
		}
	case *ast.FuncCallExpr:
		ex.Func = rewriteExpr(ex.Func, main)
		ex.Receiver = rewriteExpr(ex.Receiver, main)
		rewriteExprs(ex.Args, main)
	case *ast.LogicalOpExpr:
		ex.Lhs = rewriteExpr(ex.Lhs, main)
		ex.Rhs = rewriteExpr(ex.Rhs, main)
	case *ast.RelationalOpExpr:
		ex.Lhs = rewriteExpr(ex.Lhs, main)
		ex.Rhs = rewriteExpr(ex.Rhs, main)
	case *ast.ArithmeticOpExpr:
		ex.Lhs = rewriteExpr(ex.Lhs, main)
		ex.Rhs = rewriteExpr(ex.Rhs, main)
	case *ast.UnaryMinusOpExpr:
		ex.Expr = rewriteExpr(ex.Expr, main)
	case *ast.UnaryNotOpExpr:
		ex.Expr = rewriteExpr(ex.Expr, main)
	case *ast.UnaryLenOpExpr:
		ex.Expr = rewriteExpr(ex.Expr, main)
	case *ast.FunctionExpr:
		rewriteStmts(ex.Stmts, false)
	}
	return e
}
