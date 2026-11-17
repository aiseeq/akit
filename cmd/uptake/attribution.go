package main

import (
	"log/slog"
	"path"
	"slices"
	"strings"

	"github.com/aiseeq/akit/shellcmd"
	"mvdan.cc/sh/v3/syntax"
)

type callFlow struct {
	start, end    int
	line          int
	final         bool
	andFinal      bool
	blocked       bool
	assignSub     bool
	suppressFault bool
	truncated     bool
	pipefail      bool
	pipeline      bool
}

type flowState struct {
	final         bool
	andFinal      bool
	blocked       bool
	assignSub     bool
	suppressFault bool
	truncated     bool
	pipefail      bool
	pipeline      bool
}

type attribution struct {
	code   int
	fault  string
	output string
}

func (u *run) attributeUtility(tool, command, output string, commandCode, ordinal int) attribution {
	calls, err := shellcmd.Calls(command)
	if err != nil {
		slog.Debug("parse utility command for attribution", "error", err)
		return attribution{code: -1}
	}
	var toolCalls []shellcmd.Call
	for _, c := range calls {
		if c.Name == tool {
			toolCalls = append(toolCalls, c)
		}
	}
	if ordinal < 0 {
		ordinal = len(toolCalls) - 1
	}
	if ordinal < 0 || ordinal >= len(toolCalls) {
		return attribution{code: -1}
	}
	target := toolCalls[ordinal]
	flow, ok := commandCallFlow(command, target)
	if !ok || target.Background {
		flow.blocked = true
	}
	callOutputs := outputsForCalls(tool, command, target, calls, toolCalls, output, flow.truncated)
	callOutput := callOutputs[ordinal]
	wrapperExit := wrapperOwnsExit(target, commandCode, output)
	code := attributedUtilityCode(u, tool, flow, callOutput, commandCode, wrapperExit, target.Args)
	fault := utilityFaultArgs(u, tool, callOutput, code, wrapperExit, target.Args)
	if flow.suppressFault {
		fault = ""
	}
	fault = suppressRepeatedRefusal(u, tool, fault, callOutputs, ordinal)
	return attribution{code: code, fault: fault, output: callOutput}
}

func attributedUtilityCode(u *run, tool string, flow callFlow, output string, commandCode int, wrapperExit bool, args []string) int {
	if wrapperExit || flow.blocked {
		return -1
	}
	if flow.andFinal && commandCode == 0 {
		return 0
	}
	if flow.pipeline && flow.pipefail && commandCode != 0 {
		return -1
	}
	if flow.final {
		return commandCode
	}
	if flow.andFinal && commandCode > 0 && !flow.assignSub && (explicitToolFailureArgs(u, tool, output, args) || commandCode == 2 && hasUtilityErrorLine(u, tool, output, args)) {
		return commandCode
	}
	return -1
}

func suppressRepeatedRefusal(u *run, tool, fault string, outputs []string, ordinal int) string {
	if fault != "refusal" {
		return fault
	}
	for earlier := range ordinal {
		if explicitToolFailure(u, tool, outputs[earlier]) {
			return ""
		}
	}
	return fault
}

func commandCallFlow(command string, target shellcmd.Call) (callFlow, bool) {
	file, err := syntax.NewParser().Parse(strings.NewReader(command), "")
	if err != nil {
		slog.Debug("parse shell syntax for call attribution", "error", err)
		return callFlow{}, false
	}
	parser := flowParser{}
	parser.visitList(file.Stmts, flowState{final: true})
	outer, ok := parser.find(target)
	if !ok {
		return callFlow{}, false
	}
	if script, offset, via, found := embeddedScript(command, file, target); found {
		innerTarget := target
		innerTarget.Offset = offset
		innerTarget.Line = 1 + strings.Count(script[:offset], "\n")
		innerTarget.Via = trimScriptVia(target.Via, via)
		inner, innerOK := commandCallFlow(script, innerTarget)
		if !innerOK {
			return callFlow{}, false
		}
		if outer.final || outer.andFinal {
			inner.andFinal = (inner.andFinal || inner.final) && (outer.final || outer.andFinal)
			inner.final = inner.final && outer.final
		} else {
			inner.final, inner.andFinal = false, false
		}
		inner.blocked = inner.blocked || outer.blocked
		inner.suppressFault = inner.suppressFault || outer.suppressFault
		inner.truncated = inner.truncated || outer.truncated
		return inner, true
	}
	return outer, true
}

type flowParser struct{ flows []callFlow }

func (p *flowParser) visitList(stmts []*syntax.Stmt, state flowState) {
	for i, stmt := range stmts {
		child := state
		child.final = state.final && i == len(stmts)-1
		p.visitStmt(stmt, child)
		if enabled, ok := pipefailSetting(stmt); ok {
			state.pipefail = enabled
		}
	}
}

func (p *flowParser) visitStmt(stmt *syntax.Stmt, state flowState) {
	if stmt == nil {
		return
	}
	state.blocked = state.blocked || stmt.Negated || stmt.Background
	state.suppressFault = state.suppressFault || stmt.Negated || stmt.Background
	p.visitCommand(stmt.Cmd, state)
}

func (p *flowParser) visitCommand(command syntax.Command, state flowState) {
	switch node := command.(type) {
	case *syntax.BinaryCmd:
		p.visitBinary(node, state)
	case *syntax.CallExpr:
		p.addExpr(node, state)
	case *syntax.Subshell:
		p.visitList(node.Stmts, state)
	case *syntax.Block:
		p.visitList(node.Stmts, state)
	case *syntax.TimeClause:
		p.visitStmt(node.Stmt, state)
	default:
		state.blocked = true
		syntax.Walk(command, func(child syntax.Node) bool {
			if expr, ok := child.(*syntax.CallExpr); ok {
				callState := state
				if callInFilteredPipeline(command, expr) {
					callState.truncated = true
				}
				p.addExpr(expr, callState)
			}
			return true
		})
	}
}

func callInFilteredPipeline(root syntax.Node, call *syntax.CallExpr) bool {
	offset := int(call.Pos().Offset())
	found := false
	syntax.Walk(root, func(node syntax.Node) bool {
		pipe, ok := node.(*syntax.BinaryCmd)
		if !ok || pipe.Op != syntax.Pipe && pipe.Op != syntax.PipeAll {
			return true
		}
		start, end := int(pipe.Pos().Offset()), int(pipe.End().Offset())
		if offset >= start && offset < end && containsOutputFilter(pipe.Y.Cmd) {
			found = true
			return false
		}
		return true
	})
	return found
}

func (p *flowParser) visitBinary(node *syntax.BinaryCmd, state flowState) {
	switch node.Op {
	case syntax.AndStmt:
		state.andFinal = state.andFinal || state.final
		state.final = false
		p.visitStmt(node.X, state)
		p.visitStmt(node.Y, state)
	case syntax.OrStmt:
		state.final, state.andFinal = false, false
		p.visitStmt(node.X, state)
		p.visitStmt(node.Y, state)
	default:
		left := state
		left.blocked = true
		left.truncated = left.truncated || containsOutputFilter(node.Y.Cmd)
		left.final, left.andFinal = false, false
		left.pipeline, state.pipeline = true, true
		left.pipefail = state.pipefail
		p.visitStmt(node.X, left)
		p.visitStmt(node.Y, state)
	}
}

func (p *flowParser) addExpr(expr *syntax.CallExpr, state flowState) {
	p.flows = append(p.flows, flowFromExpr(expr, state))
	assignmentOnly := len(expr.Args) == 0 && len(expr.Assigns) > 0
	syntax.Walk(expr, func(node syntax.Node) bool {
		nested, ok := node.(*syntax.CallExpr)
		if !ok || nested == expr {
			return true
		}
		nestedState := state
		if !assignmentOnly {
			nestedState.blocked, nestedState.suppressFault = true, true
		} else {
			nestedState.assignSub = true
		}
		p.flows = append(p.flows, flowFromExpr(nested, nestedState))
		return true
	})
}

func flowFromExpr(expr *syntax.CallExpr, state flowState) callFlow {
	return callFlow{
		start: int(expr.Pos().Offset()), end: int(expr.End().Offset()), line: int(expr.Pos().Line()),
		final: state.final, andFinal: state.andFinal, blocked: state.blocked,
		assignSub: state.assignSub, suppressFault: state.suppressFault,
		truncated: state.truncated, pipefail: state.pipefail, pipeline: state.pipeline,
	}
}

func (p *flowParser) find(target shellcmd.Call) (callFlow, bool) {
	best, width, found := callFlow{}, int(^uint(0)>>1), false
	for _, candidate := range p.flows {
		if !flowContains(candidate, target) {
			continue
		}
		if candidateWidth := candidate.end - candidate.start; candidateWidth < width {
			best, width, found = candidate, candidateWidth, true
		}
	}
	return best, found
}

func flowContains(candidate callFlow, target shellcmd.Call) bool {
	if target.Offset < 0 {
		return candidate.line == target.Line
	}
	return target.Offset >= candidate.start && target.Offset < candidate.end
}

func outputsForCalls(tool, command string, target shellcmd.Call, calls, toolCalls []shellcmd.Call, output string, truncated bool) []string {
	outputs := make([]string, len(toolCalls))
	if truncated {
		return outputs
	}
	lines := outputLinesWithoutWrappers(target, output)
	lines = filterCommandOutput(tool, calls, lines)
	visible := visibleOutputCalls(toolCalls)
	if len(visible) == 0 {
		return outputs
	}
	if len(toolCalls) == 1 {
		outputs[0] = outputForSingleCall(tool, calls, toolCalls[0], lines)
		return outputs
	}
	return distributeOutputBlocks(tool, lines, toolCalls, visible, !hasTrailingVisibleOutputSource(command, calls, toolCalls))
}

func visibleOutputCalls(toolCalls []shellcmd.Call) []int {
	visible := make([]int, 0, len(toolCalls))
	for i, call := range toolCalls {
		if redirectsBothStreamsToNull(call) {
			continue
		}
		visible = append(visible, i)
	}
	return visible
}

func outputForSingleCall(tool string, calls []shellcmd.Call, target shellcmd.Call, lines []string) string {
	if len(calls) == 1 || !hasOtherOutputSource(calls, target) {
		blocks := toolOutputBlocks(tool, lines)
		if len(blocks) > 0 {
			var visible []string
			for _, block := range blocks {
				if callCanEmitOutputBlock(target, block) {
					visible = append(visible, block)
				}
			}
			if len(visible) > 0 {
				return strings.Join(visible, "")
			}
			if redirectsStdoutToNull(target) || redirectsStderrToNull(target) {
				return ""
			}
		}
		return strings.Join(lines, "\n")
	}
	blocks := toolOutputBlocks(tool, lines)
	if len(blocks) == 0 {
		return ""
	}
	sourceBefore, reachedTarget := false, false
	for _, call := range calls {
		if !reachedTarget && call.Name == target.Name {
			reachedTarget = true
			continue
		}
		if !reachedTarget && isOtherOutputSource(call.Name) {
			sourceBefore = true
		}
	}
	if sourceBefore {
		if callCanEmitOutputBlock(target, blocks[len(blocks)-1]) {
			return blocks[len(blocks)-1]
		}
		return ""
	}
	if callCanEmitOutputBlock(target, blocks[0]) {
		return blocks[0]
	}
	return ""
}

func hasOtherOutputSource(calls []shellcmd.Call, target shellcmd.Call) bool {
	for _, call := range calls {
		if isOtherOutputSource(call.Name) && call.Offset != target.Offset {
			return true
		}
	}
	return false
}

func isOtherOutputSource(name string) bool {
	return name == "cat" || name == "grep" || name == "rg"
}

func distributeOutputBlocks(tool string, lines []string, toolCalls []shellcmd.Call, visible []int, allowFallback bool) []string {
	blocks := toolOutputBlocks(tool, lines)
	if len(blocks) == len(visible) && len(visible) < len(toolCalls) {
		return assignVisibleBlocks(blocks, toolCalls, visible)
	}
	if len(blocks) == len(toolCalls) {
		return assignAlignedBlocks(blocks, toolCalls, visible)
	}
	matched := make([][]string, len(toolCalls))
	assigned := assignSuccessfulPrefixBeforeUsageError(tool, blocks, toolCalls, visible, matched)
	for i, block := range blocks {
		if assigned[i] {
			continue
		}
		assignOutputBlock(block, toolCalls, visible, matched, allowFallback)
	}
	return joinOutputBlocks(matched)
}

func assignSuccessfulPrefixBeforeUsageError(tool string, blocks []string, calls []shellcmd.Call, visible []int, matched [][]string) []bool {
	assigned := make([]bool, len(blocks))
	if tool != "sub" || len(blocks) < 2 || !strings.Contains(blocks[len(blocks)-1], "(sub -h for usage)") {
		return assigned
	}
	callIndex := 0
	for blockIndex, block := range blocks[:len(blocks)-1] {
		if !strings.Contains(block, "sub: ") || !(strings.Contains(block, " replacement in ") || strings.Contains(block, " replacements in ")) {
			break
		}
		for callIndex < len(visible) && !callCanEmitOutputBlock(calls[visible[callIndex]], block) {
			callIndex++
		}
		if callIndex == len(visible) {
			break
		}
		owner := visible[callIndex]
		matched[owner] = append(matched[owner], block)
		assigned[blockIndex] = true
		if !calls[owner].Loop {
			callIndex++
		}
	}
	return assigned
}

func assignVisibleBlocks(blocks []string, calls []shellcmd.Call, visible []int) []string {
	outputs := make([]string, len(calls))
	for i, block := range blocks {
		owner := visible[i]
		if callCanEmitOutputBlock(calls[owner], block) {
			outputs[owner] = block
		}
	}
	return outputs
}

func assignAlignedBlocks(blocks []string, calls []shellcmd.Call, visible []int) []string {
	matched := make([][]string, len(calls))
	for i, block := range blocks {
		if slices.Contains(visible, i) && callCanEmitOutputBlock(calls[i], block) {
			matched[i] = append(matched[i], block)
		}
	}
	return joinOutputBlocks(matched)
}

func assignOutputBlock(block string, calls []shellcmd.Call, visible []int, matched [][]string, allowFallback bool) {
	call := outputBlockCall(block, calls)
	if strings.Contains(block, " -h for usage)") {
		if call < 0 || call < len(matched) && len(matched[call]) != 0 {
			call = precedingUsageErrorCall(block, calls, matched)
		}
	}
	if call >= 0 && redirectsBothStreamsToNull(calls[call]) {
		return
	}
	needsVisibleOwner := call >= 0 && (!slices.Contains(visible, call) || !callCanEmitOutputBlock(calls[call], block))
	needsFallback := call < 0 && hasPartiallyNonEmittingCall(calls, block)
	if allowFallback && (needsVisibleOwner || needsFallback) {
		call = bestVisibleBlockCall(block, calls, visible)
	}
	if call >= 0 && callCanEmitOutputBlock(calls[call], block) {
		matched[call] = append(matched[call], block)
		return
	}
	if call < 0 && allowFallback {
		assignUnmatchedBlock(block, calls, visible, matched)
	}
}

func hasTrailingVisibleOutputSource(command string, calls, toolCalls []shellcmd.Call) bool {
	if len(toolCalls) == 0 {
		return false
	}
	last := -1
	for i, call := range calls {
		if call.Name == toolCalls[len(toolCalls)-1].Name {
			last = i
		}
	}
	for _, call := range calls[last+1:] {
		if isVisibleOutputSource(command, call) {
			return true
		}
	}
	return false
}

func isVisibleOutputSource(command string, call shellcmd.Call) bool {
	if !isOtherOutputSource(call.Name) {
		return false
	}
	if !readsFile(call) || isPipelineCall(command, call.Offset) {
		return false
	}
	for _, redir := range call.Redirs {
		switch redir.Op {
		case "&>":
			return false
		case ">", ">>":
			if redir.N == "" || redir.N == "1" {
				return false
			}
		case ">&":
			if (redir.N == "" || redir.N == "1") && redir.Target != "2" {
				return false
			}
		}
	}
	return true
}

func readsFile(call shellcmd.Call) bool {
	operands := 0
	for _, arg := range call.Args {
		if strings.HasPrefix(arg, "-") {
			continue
		}
		operands++
	}
	if call.Name == "cat" {
		return operands > 0
	}
	return operands > 1
}

func isPipelineCall(command string, offset int) bool {
	file, err := syntax.NewParser().Parse(strings.NewReader(command), "")
	if err != nil {
		return false
	}
	found := false
	syntax.Walk(file, func(node syntax.Node) bool {
		pipe, ok := node.(*syntax.BinaryCmd)
		if !ok || pipe.Op != syntax.Pipe && pipe.Op != syntax.PipeAll {
			return true
		}
		if offset >= int(pipe.Pos().Offset()) && offset < int(pipe.End().Offset()) {
			found = true
			return false
		}
		return true
	})
	return found
}

func assignUnmatchedBlock(block string, calls []shellcmd.Call, visible []int, matched [][]string) {
	var candidates []int
	for _, index := range visible {
		if len(matched[index]) == 0 && callCanEmitOutputBlock(calls[index], block) {
			candidates = append(candidates, index)
		}
	}
	if len(candidates) == 1 || len(candidates) > 0 && hasPartiallyNonEmittingCall(calls, block) && callArgumentMatchScore(block, calls[candidates[0]].Args) == 0 {
		matched[candidates[0]] = append(matched[candidates[0]], block)
	}
}

func joinOutputBlocks(blocks [][]string) []string {
	outputs := make([]string, len(blocks))
	for i, group := range blocks {
		outputs[i] = strings.Join(group, "")
	}
	return outputs
}

func hasPartiallyNonEmittingCall(calls []shellcmd.Call, block string) bool {
	for _, call := range calls {
		if !redirectsBothStreamsToNull(call) && !callCanEmitOutputBlock(call, block) {
			return true
		}
	}
	return false
}

func callCanEmitOutputBlock(call shellcmd.Call, block string) bool {
	stdoutNull := redirectsStdoutToNull(call)
	stderrNull := redirectsStderrToNull(call)
	errorOutput := utilityErrorOutputBlock(call.Name, block)
	return !(stdoutNull && !errorOutput || stderrNull && errorOutput)
}

func bestVisibleBlockCall(block string, calls []shellcmd.Call, visible []int) int {
	best, bestScore := -1, 0
	for _, index := range visible {
		if !callCanEmitOutputBlock(calls[index], block) {
			continue
		}
		score := callArgumentMatchScore(block, calls[index].Args)
		if score > bestScore {
			best, bestScore = index, score
		} else if score > 0 && score == bestScore {
			best = -1
		}
	}
	return best
}

func precedingUsageErrorCall(block string, calls []shellcmd.Call, matched [][]string) int {
	if !strings.Contains(block, " -h for usage)") || len(calls) < 2 {
		return -1
	}
	for i, call := range calls {
		if len(matched[i]) != 0 || !callMayHaveUsageError(call) || !callCanEmitOutputBlock(call, block) {
			continue
		}
		return i
	}
	return -1
}

func callMayHaveUsageError(call shellcmd.Call) bool {
	if slices.Contains(call.Args, "-h") {
		return false
	}
	if call.Name == "sub" {
		if slices.Contains(call.Args, "-F") {
			return true
		}
		return len(call.Args) == 0
	}
	return true
}

func redirectsBothStreamsToNull(call shellcmd.Call) bool {
	stdoutNull, stderrNull := false, false
	for _, redir := range call.Redirs {
		switch redir.Op {
		case "&>":
			if redir.Target == "/dev/null" {
				stdoutNull, stderrNull = true, true
			}
		case ">", ">>":
			if redir.N == "2" {
				stderrNull = redir.Target == "/dev/null"
			} else if redir.N == "" || redir.N == "1" {
				stdoutNull = redir.Target == "/dev/null"
			}
		case ">&":
			switch {
			case redir.N == "2" && redir.Target == "1":
				stderrNull = stdoutNull
			case (redir.N == "" || redir.N == "1") && redir.Target == "2":
				stdoutNull = stderrNull
			case (redir.N == "" || redir.N == "1") && redir.Target == "/dev/null":
				stdoutNull, stderrNull = true, true
			case redir.N == "2" && redir.Target == "/dev/null":
				stderrNull = true
			}
		}
	}
	return stdoutNull && stderrNull
}

func redirectsStdoutToNull(call shellcmd.Call) bool {
	stdoutNull, stderrNull := false, false
	for _, redir := range call.Redirs {
		switch redir.Op {
		case "&>":
			if redir.Target == "/dev/null" {
				stdoutNull, stderrNull = true, true
			}
		case ">", ">>":
			if redir.N == "" || redir.N == "1" {
				stdoutNull = redir.Target == "/dev/null"
			} else if redir.N == "2" {
				stderrNull = redir.Target == "/dev/null"
			}
		case ">&":
			if redir.N == "2" && redir.Target == "1" {
				stderrNull = stdoutNull
			} else if (redir.N == "" || redir.N == "1") && redir.Target == "2" {
				stdoutNull = stderrNull
			} else if (redir.N == "" || redir.N == "1") && redir.Target == "/dev/null" {
				stdoutNull, stderrNull = true, true
			} else if redir.N == "2" && redir.Target == "/dev/null" {
				stderrNull = true
			}
		}
	}
	return stdoutNull
}

func redirectsStderrToNull(call shellcmd.Call) bool {
	stdoutNull, stderrNull := false, false
	for _, redir := range call.Redirs {
		switch redir.Op {
		case "&>":
			if redir.Target == "/dev/null" {
				stdoutNull, stderrNull = true, true
			}
		case ">", ">>":
			if redir.N == "2" {
				stderrNull = redir.Target == "/dev/null"
			} else if redir.N == "" || redir.N == "1" {
				stdoutNull = redir.Target == "/dev/null"
			}
		case ">&":
			if redir.N == "2" && redir.Target == "1" {
				stderrNull = stdoutNull
			} else if redir.N == "2" && redir.Target == "/dev/null" {
				stderrNull = true
			} else if (redir.N == "" || redir.N == "1") && redir.Target == "/dev/null" {
				stdoutNull, stderrNull = true, true
			}
		}
	}
	return stderrNull
}

func utilityErrorOutputBlock(tool, block string) bool {
	if strings.Contains(block, "("+tool+" -h for usage)") {
		return true
	}
	for line := range strings.SplitSeq(block, "\n") {
		if strings.HasPrefix(line, tool+": ") {
			return strings.Contains(line, "nothing changed") || strings.Contains(line, "not found") ||
				strings.Contains(line, "invalid") || strings.Contains(line, "error") || strings.Contains(line, "unknown") ||
				strings.Contains(line, "need ") || strings.Contains(line, "empty") || strings.Contains(line, "mismatch")
		}
	}
	return false
}

func outputLinesWithoutWrappers(target shellcmd.Call, output string) []string {
	var lines []string
	for line := range strings.SplitSeq(output, "\n") {
		if !wrapperOutputLine(target, line) {
			lines = append(lines, line)
		}
	}
	return lines
}

func filterCommandOutput(tool string, calls []shellcmd.Call, lines []string) []string {
	for _, call := range calls {
		if call.Name == "echo" || call.Name == "printf" {
			lines = filterEchoLines(tool, call.Args, lines)
		}
	}
	return lines
}

func toolOutputBlocks(tool string, lines []string) []string {
	var blocks []string
	var current strings.Builder
	translationPending := false
	flush := func() {
		if current.Len() > 0 {
			blocks = append(blocks, current.String())
			current.Reset()
		}
	}
	for _, line := range lines {
		if strings.Contains(line, "("+tool+" -h for usage)") && current.Len() == 0 && len(blocks) > 0 {
			var block strings.Builder
			block.WriteString(blocks[len(blocks)-1])
			block.WriteString(line)
			block.WriteByte('\n')
			blocks[len(blocks)-1] = block.String()
			continue
		}
		if strings.HasPrefix(line, tool+": ") {
			if strings.Contains(line, "read FILE OLD/NEW form; translated to SEARCH/REPLACE:") {
				flush()
				translationPending = true
			} else if translationPending {
				translationPending = false
			} else {
				flush()
			}
			current.WriteString(line)
			current.WriteByte('\n')
			continue
		}
		if current.Len() > 0 && (translationPending || strings.Contains(line, "("+tool+" -h for usage)") || strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") || strings.HasPrefix(line, "<<<<<<<") || strings.HasPrefix(line, "=======") || strings.HasPrefix(line, ">>>>>>>")) {
			current.WriteString(line)
			current.WriteByte('\n')
		} else if current.Len() > 0 {
			flush()
		}
	}
	flush()
	return blocks
}

func filterEchoLines(tool string, args, lines []string) []string {
	filtered := lines
	for _, arg := range args {
		if !strings.Contains(arg, tool+": ") {
			continue
		}
		kept := filtered[:0]
		for _, line := range filtered {
			if !strings.Contains(arg, line) {
				kept = append(kept, line)
			}
		}
		filtered = kept
	}
	return filtered
}

func outputBlockCall(block string, calls []shellcmd.Call) int {
	match := -1
	bestScore := 0
	for i, call := range calls {
		blockForMatch := block
		blockForMatch = strings.ReplaceAll(blockForMatch, "("+calls[i].Name+" -h for usage)", "")
		for line := range strings.SplitSeq(block, "\n") {
			if strings.Contains(line, " -h for usage)") {
				blockForMatch = strings.ReplaceAll(blockForMatch, line, "")
			}
		}
		score := callArgumentMatchScore(blockForMatch, call.Args)
		if score == 0 || score < bestScore {
			continue
		}
		if score == bestScore && match >= 0 && match != i {
			return -1
		}
		match, bestScore = i, score
	}
	return match
}

func callArgumentMatchScore(line string, args []string) int {
	score := 0
	for _, arg := range args {
		if arg != "" && lineMentionsArgument(line, arg) {
			score++
		}
	}
	return score
}

func lineMentionsArgument(line, arg string) bool {
	for token := range strings.FieldsSeq(line) {
		token = strings.Trim(token, "'\"`.,:;()[]{}")
		if token == arg {
			return true
		}
	}
	return strings.Contains(line, arg) && (strings.HasPrefix(arg, "/") || len(arg) > 1 && strings.ContainsAny(arg, "/._-"))
}

func wrapperOutput(target shellcmd.Call, output string) bool {
	for _, via := range target.Via {
		if strings.Contains(output, via+": ") || via == "graft" && strings.Contains(output, "graft: exec:") {
			return true
		}
	}
	return false
}

func wrapperOutputLine(target shellcmd.Call, line string) bool {
	for _, via := range target.Via {
		if strings.HasPrefix(line, via+": ") || via == "graft" && strings.HasPrefix(line, "graft: exec:") {
			return true
		}
	}
	return false
}

func wrapperOwnsExit(call shellcmd.Call, code int, output string) bool {
	for _, via := range call.Via {
		switch via {
		case "graft":
			if code == 125 || code == 137 {
				return true
			}
		case "timeout":
			if code == 124 || code >= 125 && code <= 127 || code == 137 {
				return true
			}
		case "ssh":
			if code == 255 {
				return true
			}
		}
	}
	return slices.Contains(call.Via, "sudo") && wrapperOutput(call, output) && !strings.Contains(output, call.Name+": ")
}

func containsOutputFilter(command syntax.Command) bool {
	found := false
	syntax.Walk(command, func(node syntax.Node) bool {
		call, ok := node.(*syntax.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		name := path.Base(call.Args[0].Lit())
		if slices.Contains([]string{"head", "tail", "cut", "grep", "sed", "awk"}, name) {
			found = true
			return false
		}
		return true
	})
	return found
}

func hasOutputFilterPipeline(command string) bool {
	file, err := syntax.NewParser().Parse(strings.NewReader(command), "")
	if err != nil {
		slog.Debug("parse command pipeline for truncation", "error", err)
		return false
	}
	found := false
	syntax.Walk(file, func(node syntax.Node) bool {
		binary, ok := node.(*syntax.BinaryCmd)
		if ok && (binary.Op == syntax.Pipe || binary.Op == syntax.PipeAll) && containsOutputFilter(binary.Y.Cmd) {
			found = true
			return false
		}
		return !found
	})
	return found
}

func pipefailSetting(stmt *syntax.Stmt) (bool, bool) {
	call, ok := stmt.Cmd.(*syntax.CallExpr)
	if !ok || len(call.Args) != 3 || call.Args[0].Lit() != "set" || call.Args[2].Lit() != "pipefail" {
		return false, false
	}
	switch call.Args[1].Lit() {
	case "-o":
		return true, true
	case "+o":
		return false, true
	default:
		return false, false
	}
}

func embeddedScript(command string, file *syntax.File, target shellcmd.Call) (string, int, []string, bool) {
	var foundScript string
	var foundOffset int
	var foundVia []string
	width := int(^uint(0) >> 1)
	syntax.Walk(file, func(node syntax.Node) bool {
		call, ok := node.(*syntax.CallExpr)
		if !ok || target.Offset >= 0 && (target.Offset < int(call.Pos().Offset()) || target.Offset >= int(call.End().Offset())) {
			return true
		}
		script, offset, via, found := embeddedScriptCandidate(command, call, target)
		if found {
			candidateWidth := int(call.End().Offset() - call.Pos().Offset())
			if candidateWidth < width {
				foundScript, foundOffset, foundVia, width = script, offset, via, candidateWidth
			}
		}
		return true
	})
	return foundScript, foundOffset, foundVia, foundScript != ""
}

func embeddedScriptCandidate(command string, call *syntax.CallExpr, target shellcmd.Call) (string, int, []string, bool) {
	word, via := embeddedScriptWord(call, target.Via)
	if word == nil {
		return "", 0, nil, false
	}
	script, contentStart, ok := scriptWordContents(command, word)
	if !ok {
		return "", 0, nil, false
	}
	offset := target.Offset - contentStart
	if offset < 0 || offset >= len(script) || !scriptCallAt(script, offset, target.Name, target.Args) {
		offset = uniqueScriptCallOffset(script, target.Name, target.Args)
	}
	if offset < 0 {
		return "", 0, nil, false
	}
	return script, offset, via, true
}

func embeddedScriptWord(call *syntax.CallExpr, via []string) (*syntax.Word, []string) {
	if len(call.Args) == 0 || len(via) == 0 {
		return nil, nil
	}
	argsByName := make(map[string][]int, len(call.Args))
	for argIndex, arg := range call.Args {
		name := path.Base(arg.Lit())
		argsByName[name] = append(argsByName[name], argIndex)
	}
	for viaIndex, name := range via {
		if name != "bash" && name != "sh" && name != "ssh" {
			continue
		}
		argIndex := firstIndexAtOrAfter(argsByName[name], 0)
		if argIndex < 0 {
			continue
		}
		if name == "ssh" {
			return call.Args[len(call.Args)-1], scriptWrapperVia(via, viaIndex, false)
		}
		if word, hasCommandFlag := shellCommandScriptWord(call.Args, argIndex); word != nil {
			return word, scriptWrapperVia(via, viaIndex, hasCommandFlag)
		}
	}
	return nil, nil
}

func firstIndexAtOrAfter(indices []int, min int) int {
	for _, index := range indices {
		if index >= min {
			return index
		}
	}
	return -1
}

func shellCommandScriptWord(args []*syntax.Word, shellIndex int) (*syntax.Word, bool) {
	for flagIndex := shellIndex + 1; flagIndex+1 < len(args); flagIndex++ {
		flag := args[flagIndex].Lit()
		if flag == "-c" || len(flag) > 2 && strings.HasPrefix(flag, "-") && !strings.HasPrefix(flag, "--") && strings.Contains(flag[1:], "c") {
			return args[flagIndex+1], true
		}
	}
	return nil, false
}

func scriptWrapperVia(via []string, shellIndex int, hasCommandFlag bool) []string {
	wrapper := slices.Clone(via[:shellIndex+1])
	if hasCommandFlag {
		wrapper = append(wrapper, "-c")
	}
	return wrapper
}

func scriptWordContents(command string, word *syntax.Word) (string, int, bool) {
	script := word.Lit()
	start, end := int(word.Pos().Offset()), int(word.End().Offset())
	if start < 0 || end > len(command) || start >= end {
		return "", 0, false
	}
	raw := command[start:end]
	if script == "" {
		switch {
		case strings.HasPrefix(raw, "'") && strings.HasSuffix(raw, "'"):
			script = raw[1 : len(raw)-1]
		case strings.HasPrefix(raw, "\"") && strings.HasSuffix(raw, "\""):
			script = raw[1 : len(raw)-1]
		default:
			return "", 0, false
		}
	}
	contentStart := start
	if command[contentStart] == '\'' || command[contentStart] == '"' {
		contentStart++
	}
	return script, contentStart, true
}

func scriptCallAt(script string, offset int, name string, args []string) bool {
	calls, err := shellcmd.Calls(script)
	if err != nil {
		slog.Debug("parse embedded script calls", "error", err)
		return false
	}
	for _, call := range calls {
		if call.Offset == offset && call.Name == name && slices.Equal(call.Args, args) {
			return true
		}
	}
	return false
}

func uniqueScriptCallOffset(script string, name string, args []string) int {
	calls, err := shellcmd.Calls(script)
	if err != nil {
		slog.Debug("parse embedded script calls for unique call", "error", err)
		return -1
	}
	match := -1
	for _, call := range calls {
		if call.Name != name || !slices.Equal(call.Args, args) {
			continue
		}
		if match >= 0 {
			return -1
		}
		match = call.Offset
	}
	return match
}

func trimScriptVia(via, wrapper []string) []string {
	if len(via) < len(wrapper) {
		return nil
	}
	if !slices.Equal(via[:len(wrapper)], wrapper) {
		return nil
	}
	return slices.Clone(via[len(wrapper):])
}

func (u *run) callHasTruncator(tool, command string, ordinal int) bool {
	calls, err := shellcmd.Calls(command)
	if err != nil {
		slog.Debug("parse command for call truncation", "error", err)
		return false
	}
	var toolCalls []shellcmd.Call
	for _, call := range calls {
		if call.Name == tool {
			toolCalls = append(toolCalls, call)
		}
	}
	if ordinal < 0 || ordinal >= len(toolCalls) {
		return false
	}
	flow, ok := commandCallFlow(command, toolCalls[ordinal])
	return ok && flow.truncated
}

func explicitToolFailure(u *run, tool, output string) bool {
	return explicitToolFailureArgs(u, tool, output, nil)
}

func explicitToolFailureArgs(u *run, tool, output string, args []string) bool {
	return u.explicitRefusalArgs(tool, output, args)
}

func hasUtilityErrorLine(u *run, tool, output string, args []string) bool {
	for line := range strings.SplitSeq(output, "\n") {
		if strings.HasPrefix(line, tool+": ") && !u.explicitSuccess(tool, line) && !(tool == "tl" && strings.Contains(line, "tl: no events")) {
			if tool == "mut" && strings.HasPrefix(line, "mut: outer graft exec --heavy is unnecessary") {
				continue
			}
			message := line
			for _, arg := range args {
				message = stripArgumentText(message, arg)
			}
			message = strings.ToLower(message)
			if strings.TrimSpace(strings.TrimPrefix(message, tool+": ")) != "" {
				return true
			}
		}
	}
	return false
}

func utilityFaultArgs(u *run, tool, output string, code int, wrapperExit bool, args []string) string {
	if wrapperExit {
		return ""
	}
	_, toolFaultLines, usageMarkers := u.toolOutputFlags(tool, output)
	framesAkit := strings.Contains(output, "github.com/aiseeq/akit/") && strings.Contains(output, ".go:")
	trimmed := strings.TrimSpace(output)
	startsPanic := strings.HasPrefix(trimmed, "panic:") || strings.HasPrefix(trimmed, "fatal error:")
	if toolFaultLines > 0 || startsPanic && framesAkit {
		return "crash"
	}
	if usageMarkers > 0 && hasUtilityErrorLine(u, tool, output, args) {
		return "call-error"
	}
	if code == 2 && hasUtilityErrorLine(u, tool, output, args) {
		return "call-error"
	}
	if code >= 128 && tool != "tk" {
		return "killed"
	}
	if code > 0 && tool == "tl" && strings.Contains(output, "tl: no events") {
		return "refusal"
	}
	if code > 0 && hasUtilityErrorLine(u, tool, output, args) {
		return "refusal"
	}
	if u.explicitRefusalArgs(tool, output, args) {
		return "refusal"
	}
	return ""
}
