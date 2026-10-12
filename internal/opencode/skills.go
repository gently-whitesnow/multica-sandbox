package opencode

import (
	"fmt"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/gently-whitesnow/multica-sandbox/internal/multica"
)

// Skill handling mirrors Multica v0.5.2 server/internal/daemon/execenv/{context,
// skill_visibility,sidecar_manifest}.go and runtime_config_sections.go (writeSkills,
// writeSubIssueCreation, issueContractsSkill). Re-check on upgrades.

// SkillsDir is OpenCode's global skill root under XDG_CONFIG_HOME; attempts start with it empty.
const SkillsDir = "/workspace/config/opencode/skills"

// Claim bounds: the upstream claim carries inline skills without its own limits.
const (
	maxSkills       = 64
	maxSkillFiles   = 256
	maxSkillFile    = 1 << 20
	maxSkillsTotal  = 8 << 20
	maxSkillPathLen = 512
)

const platformSkillName = "multica-platform"

// legacyIssueSkillName is the platform skill's pre-merge name; v0.5.2 still points at it.
const legacyIssueSkillName = "multica-working-on-issues"

var nonAlphaNum = regexp.MustCompile(`[^a-z0-9]+`)

func sanitizeSkillName(name string) string {
	s := strings.ToLower(strings.TrimSpace(name))
	s = nonAlphaNum.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if s == "" {
		s = "skill"
	}
	return s
}

func skillSlugCandidate(base string, attempt int) string {
	switch {
	case attempt <= 0:
		return base
	case attempt == 1:
		return base + "-multica"
	default:
		return fmt.Sprintf("%s-multica-%d", base, attempt)
	}
}

// resolveSkillSlugs assigns on-disk slugs, deduplicated within the batch in index order.
// Attempts start with an empty skill root, so upstream's filesystem collision probe
// (allocateCollisionFreeSkillDir) always keeps these slugs.
func resolveSkillSlugs(skills []multica.Skill) []string {
	slugs := make([]string, len(skills))
	taken := make(map[string]struct{}, len(skills))
	for i, skill := range skills {
		base := sanitizeSkillName(skill.Name)
		slug := base
		for attempt := 1; ; attempt++ {
			if _, clash := taken[slug]; !clash {
				break
			}
			slug = skillSlugCandidate(base, attempt)
		}
		taken[slug] = struct{}{}
		slugs[i] = slug
	}
	return slugs
}

// modelVisibleSkills lists invocable skills by slug, honouring disable-model-invocation.
func modelVisibleSkills(skills []multica.Skill) []multica.Skill {
	if len(skills) == 0 {
		return nil
	}
	slugs := resolveSkillSlugs(skills)
	visible := make([]multica.Skill, 0, len(skills))
	for i, skill := range skills {
		if !skillDisablesModelInvocation(skill.Content) {
			skill.Name = slugs[i]
			visible = append(visible, skill)
		}
	}
	return visible
}

func skillDisablesModelInvocation(content string) bool {
	fmBody, _, ok := frontmatterParts(content)
	if !ok || strings.TrimSpace(fmBody) == "" {
		return false
	}
	var data map[string]any
	if err := yaml.Unmarshal([]byte(fmBody), &data); err != nil {
		return false
	}
	switch v := data["disable-model-invocation"].(type) {
	case bool:
		return v
	case string:
		return strings.EqualFold(strings.TrimSpace(v), "true")
	}
	return false
}

// skillTree returns the files to lay down under SkillsDir, keyed by relative path, like
// upstream writeSkillFiles; it rejects claims beyond the bounds or escaping their skill.
func skillTree(skills []multica.Skill) (map[string][]byte, error) {
	if len(skills) > maxSkills {
		return nil, fmt.Errorf("claim carries more than %d skills: %w", maxSkills, ErrDenied)
	}
	files, dirs, total := map[string][]byte{}, map[string]bool{}, 0
	add := func(name string, data string) error {
		if len(data) > maxSkillFile || len(name) > maxSkillPathLen {
			return fmt.Errorf("skill file exceeds limits: %w", ErrDenied)
		}
		if total += len(data); total > maxSkillsTotal {
			return fmt.Errorf("skills exceed %d bytes: %w", maxSkillsTotal, ErrDenied)
		}
		// In either order: a/b then a, or a then a/b.
		for dir := path.Dir(name); dir != "."; dir = path.Dir(dir) {
			if _, file := files[dir]; file {
				return fmt.Errorf("skill file path is also a directory: %w", ErrDenied)
			}
			dirs[dir] = true
		}
		if dirs[name] {
			return fmt.Errorf("skill file path is also a directory: %w", ErrDenied)
		}
		if _, exists := files[name]; exists {
			return fmt.Errorf("skill lists a file twice: %w", ErrDenied)
		}
		files[name] = []byte(data)
		return nil
	}
	for i, slug := range resolveSkillSlugs(skills) {
		skill := skills[i]
		if len(skill.Files) > maxSkillFiles {
			return nil, fmt.Errorf("skill carries more than %d files: %w", maxSkillFiles, ErrDenied)
		}
		if err := add(slug+"/SKILL.md", ensureSkillFrontmatter(skill.Content, slug, skill.Description)); err != nil {
			return nil, err
		}
		for _, f := range skill.Files {
			if strings.EqualFold(filepath.Clean(f.Path), "SKILL.md") {
				continue
			}
			clean := path.Clean(f.Path)
			if f.Path == "" || strings.ContainsAny(f.Path, "\\\x00") || path.IsAbs(clean) || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
				return nil, fmt.Errorf("skill file path escapes its skill: %w", ErrDenied)
			}
			if err := add(slug+"/"+clean, f.Content); err != nil {
				return nil, err
			}
		}
	}
	return files, nil
}

// ensureSkillFrontmatter returns SKILL.md content leading with frontmatter whose name is
// the slug; every other key stays byte-identical (upstream MUL-5529).
func ensureSkillFrontmatter(content, slug, description string) string {
	fmStart, ok := frontmatterBodyStart(content)
	if !ok {
		return synthesizeFrontmatter(content, slug, description)
	}
	if isFrontmatterValidYAML(content) {
		if frontmatterHasNameKey(content) {
			if rewritten, verified := setFrontmatterName(content, fmStart, slug); verified {
				return rewritten
			}
			if rebuilt, ok := renameFrontmatterNameViaNode(content, slug); ok {
				return rebuilt
			}
			_, body, _ := frontmatterParts(content)
			return synthesizeFrontmatter(body, slug, description)
		}
		return content[:fmStart] + "name: " + slug + "\n" + content[fmStart:]
	}
	if hasFrontmatterName(content[fmStart:]) {
		_, body, _ := frontmatterParts(content)
		return synthesizeFrontmatter(body, slug, description)
	}
	return content[:fmStart] + "name: " + slug + "\n" + content[fmStart:]
}

func frontmatterMapping(fmBody string) (*yaml.Node, *yaml.Node, bool) {
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(fmBody), &doc); err != nil {
		return nil, nil, false
	}
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, nil, false
	}
	return &doc, doc.Content[0], true
}

func frontmatterHasNameKey(content string) bool {
	fmBody, _, ok := frontmatterParts(content)
	if !ok {
		return false
	}
	_, mapping, ok := frontmatterMapping(fmBody)
	if !ok {
		return false
	}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == "name" {
			return true
		}
	}
	return false
}

func synthesizeFrontmatter(body, slug, description string) string {
	var b strings.Builder
	b.WriteString("---\n")
	fmt.Fprintf(&b, "name: %s\n", slug)
	if d := strings.TrimSpace(description); d != "" {
		fmt.Fprintf(&b, "description: %s\n", yamlEscapeInline(d))
	}
	b.WriteString("---\n\n")
	b.WriteString(body)
	return b.String()
}

func isFrontmatterValidYAML(content string) bool {
	fmBody, _, ok := frontmatterParts(content)
	if !ok || strings.TrimSpace(fmBody) == "" {
		return false
	}
	var m map[string]any
	return yaml.Unmarshal([]byte(fmBody), &m) == nil
}

// frontmatterParts splits the YAML block from the body; a block without a closing
// `---` line keeps the full content as body.
func frontmatterParts(content string) (fmBody, body string, ok bool) {
	start, ok := frontmatterBodyStart(content)
	if !ok {
		return "", content, false
	}
	rest := content[start:]
	for searchFrom := 0; ; {
		nl := strings.Index(rest[searchFrom:], "\n---")
		if nl < 0 {
			return "", content, false
		}
		closeAt := searchFrom + nl
		after := rest[closeAt+len("\n---"):]
		switch {
		case after == "" || after == "\r":
			return rest[:closeAt], "", true
		case strings.HasPrefix(after, "\n"):
			return rest[:closeAt], after[len("\n"):], true
		case strings.HasPrefix(after, "\r\n"):
			return rest[:closeAt], after[len("\r\n"):], true
		default:
			searchFrom = closeAt + len("\n---")
		}
	}
}

func frontmatterBodyStart(content string) (int, bool) {
	if strings.HasPrefix(content, "---\n") {
		return 4, true
	}
	if strings.HasPrefix(content, "---\r\n") {
		return 5, true
	}
	return 0, false
}

// hasFrontmatterName is the lexical twin of frontmatterHasNameKey for unparseable blocks.
func hasFrontmatterName(fmBody string) bool {
	closeIdx := strings.Index(fmBody, "\n---")
	if closeIdx < 0 {
		closeIdx = len(fmBody)
	}
	for _, line := range strings.Split(fmBody[:closeIdx], "\n") {
		line = strings.TrimSuffix(line, "\r")
		if strings.HasPrefix(line, "name:") || strings.HasPrefix(line, `"name":`) || strings.HasPrefix(line, `'name':`) {
			return true
		}
	}
	return false
}

// frontmatterNameValueSpan returns the byte range of the top-level name entry, bounded
// by the parser's line of the next key rather than by indentation.
func frontmatterNameValueSpan(fmBody string) (start, end int, ok bool) {
	closeIdx := strings.Index(fmBody, "\n---")
	if closeIdx < 0 {
		closeIdx = len(fmBody)
	}
	block := fmBody[:closeIdx]
	_, mapping, ok := frontmatterMapping(block)
	if !ok {
		return 0, 0, false
	}
	nameLine, nextKeyLine := 0, 0
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value != "name" {
			continue
		}
		nameLine = mapping.Content[i].Line
		if i+2 < len(mapping.Content) {
			nextKeyLine = mapping.Content[i+2].Line
		}
		break
	}
	if nameLine == 0 {
		return 0, 0, false
	}
	lines := strings.Split(block, "\n")
	lastLine := len(lines)
	if nextKeyLine > 0 {
		lastLine = min(nextKeyLine-1, len(lines))
	}
	for lastLine > nameLine {
		line := strings.TrimSuffix(lines[lastLine-1], "\r")
		if strings.TrimSpace(line) != "" && !strings.HasPrefix(line, "#") {
			break
		}
		lastLine--
	}
	if lastLine < nameLine {
		return 0, 0, false
	}
	offset := 0
	for i, line := range lines {
		if i == nameLine-1 {
			start = offset
		}
		if i == lastLine-1 {
			return start, offset + len(line), true
		}
		offset += len(line) + 1
	}
	return 0, 0, false
}

func setFrontmatterName(content string, fmStart int, slug string) (string, bool) {
	fmBody := content[fmStart:]
	start, end, ok := frontmatterNameValueSpan(fmBody)
	if !ok {
		return "", false
	}
	replacement := "name: " + slug
	if strings.HasSuffix(fmBody[start:end], "\r") {
		replacement += "\r"
	}
	rewritten := content[:fmStart+start] + replacement + content[fmStart+end:]
	if !frontmatterNameIs(rewritten, slug) {
		return "", false
	}
	return rewritten, true
}

// renameFrontmatterNameViaNode rebuilds the block from its node with name set to slug;
// aliases of the name keep the value they resolved to before the rename.
func renameFrontmatterNameViaNode(content, slug string) (string, bool) {
	fmBody, body, ok := frontmatterParts(content)
	if !ok {
		return "", false
	}
	doc, mapping, ok := frontmatterMapping(fmBody)
	if !ok {
		return "", false
	}
	var value *yaml.Node
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == "name" {
			value = mapping.Content[i+1]
			break
		}
	}
	if value == nil {
		return "", false
	}
	if value.Anchor != "" {
		materializeAliasesOf(doc, value)
	}
	value.Kind, value.Tag, value.Style, value.Value, value.Content, value.Anchor = yaml.ScalarNode, "!!str", 0, slug, nil, ""
	marshaled, err := yaml.Marshal(doc)
	if err != nil {
		return "", false
	}
	rebuilt := "---\n" + string(marshaled) + "---\n" + body
	if !frontmatterNameIs(rebuilt, slug) {
		return "", false
	}
	return rebuilt, true
}

func materializeAliasesOf(root, target *yaml.Node) {
	for _, child := range root.Content {
		if child.Kind == yaml.AliasNode && child.Alias == target {
			*child = *cloneYAMLNode(target)
			continue
		}
		materializeAliasesOf(child, target)
	}
}

func cloneYAMLNode(n *yaml.Node) *yaml.Node {
	clone := *n
	clone.Anchor, clone.Alias = "", nil
	if len(n.Content) > 0 {
		clone.Content = make([]*yaml.Node, len(n.Content))
		for i, child := range n.Content {
			clone.Content[i] = cloneYAMLNode(child)
		}
	}
	return &clone
}

func frontmatterNameIs(content, want string) bool {
	fmBody, _, ok := frontmatterParts(content)
	if !ok {
		return false
	}
	var m map[string]any
	if err := yaml.Unmarshal([]byte(fmBody), &m); err != nil {
		return false
	}
	name, _ := m["name"].(string)
	return name == want
}

// yamlEscapeInline returns a double-quoted single-line YAML string.
func yamlEscapeInline(s string) string {
	flat := strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ").Replace(s)
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(flat) + `"`
}

func builtinSlug(skills []multica.Skill, name string) (string, bool) {
	for _, skill := range skills {
		if skill.Name == name {
			return skill.Name, true
		}
	}
	return "", false
}

// writeSubIssueCreation is upstream writeSubIssueCreation.
func writeSubIssueCreation(b *strings.Builder, skills []multica.Skill) {
	b.WriteString("## Sub-issue Creation\n\n")
	b.WriteString("`--status todo` starts an agent-assigned child immediately; `--status backlog` parks it for later promotion; `--stage <N>` groups children into ordered stages.")
	if where, ok := issueContractsSkill(modelVisibleSkills(skills)); ok {
		b.WriteString(" Before creating sub-issues, read " + where + " — it covers serial chains, promotion, and stage wake semantics.")
	}
	b.WriteString("\n\n")
}

// issueContractsSkill is upstream v0.5.2 issueContractsSkill: the platform skill, else
// the pre-merge skill, else no pointer.
func issueContractsSkill(skills []multica.Skill) (string, bool) {
	if slug, ok := builtinSlug(skills, platformSkillName); ok {
		return "`references/issues.md` in the `" + slug + "` skill", true
	}
	if slug, ok := builtinSlug(skills, legacyIssueSkillName); ok {
		return "the `" + slug + "` skill", true
	}
	return "", false
}

// writeSkills is upstream writeSkills: an index of invocable skill names.
func writeSkills(b *strings.Builder, all []multica.Skill) {
	skills := modelVisibleSkills(all)
	if len(skills) == 0 {
		return
	}
	b.WriteString("## Skills\n\n")
	b.WriteString("You have the following skills installed (discovered automatically):\n\n")
	for _, skill := range skills {
		fmt.Fprintf(b, "- **%s**\n", skill.Name)
	}
	b.WriteString("\n")
	if slug, _ := builtinSlug(skills, platformSkillName); slug != "" {
		b.WriteString("For a Multica platform action this brief does not fully cover — issue and PR contracts, mentions, agents, squads, autopilots, projects, runtimes, skill import — load the `" + slug + "` skill and open the reference(s) its routing table names for the domains your task touches.\n\n")
	}
}

var slashSkill = regexp.MustCompile(`\[/((?:[^\]\\]|\\.)+)\]\(slash://skill/([^)]+)\)`)

// selectedSkills is upstream's explicit chat skill selection: display names of agent
// skills referenced as slash links, in message order, once each.
func selectedSkills(t multica.Task) []string {
	names := map[string]string{}
	for _, s := range t.Agent.Skills {
		names[s.ID] = s.Name
	}
	var selected []string
	seen := map[string]bool{}
	for _, m := range slashSkill.FindAllStringSubmatch(t.ChatMessage, -1) {
		if name, ok := names[m[2]]; ok && !seen[m[2]] {
			seen[m[2]] = true
			selected = append(selected, name)
		}
	}
	return selected
}
