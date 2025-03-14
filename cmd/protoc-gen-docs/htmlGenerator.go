// Copyright 2018 Istio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this currentFile except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"github.com/client9/gospell"
	"google.golang.org/genproto/googleapis/api/annotations"
	"google.golang.org/protobuf/proto"
	descriptor "google.golang.org/protobuf/types/descriptorpb"
	plugin "google.golang.org/protobuf/types/pluginpb"

	"istio.io/tools/pkg/markdown"
	"istio.io/tools/pkg/protomodel"
)

type outputMode int

const (
	htmlPage                    outputMode = iota // stand-alone HTML page
	htmlFragment                                  // core portion of an HTML body, no head section or other wrappers
	htmlFragmentWithFrontMatter                   // like a fragment, but with YAML front-matter
)

type htmlGenerator struct {
	buffer           bytes.Buffer
	model            *protomodel.Model
	mode             outputMode
	numWarnings      int
	speller          *gospell.GoSpell
	customStyleSheet string

	// transient state as individual files are processed
	currentPackage             *protomodel.PackageDescriptor
	currentFrontMatterProvider *protomodel.FileDescriptor
	grouping                   bool

	genWarnings      bool
	warningsAsErrors bool
	emitYAML         bool
	camelCaseFields  bool
	perFile          bool
}

const (
	deprecated = "deprecated "
)

func newHTMLGenerator(model *protomodel.Model, mode outputMode, genWarnings bool, warningsAsErrors bool, speller *gospell.GoSpell,
	emitYAML bool, camelCaseFields bool, customStyleSheet string, perFile bool,
) *htmlGenerator {
	return &htmlGenerator{
		model:            model,
		mode:             mode,
		speller:          speller,
		genWarnings:      genWarnings,
		warningsAsErrors: warningsAsErrors,
		emitYAML:         emitYAML,
		camelCaseFields:  camelCaseFields,
		customStyleSheet: customStyleSheet,
		perFile:          perFile,
	}
}

func (g *htmlGenerator) getFileContents(file *protomodel.FileDescriptor,
	messages *[]*protomodel.MessageDescriptor,
	enums *[]*protomodel.EnumDescriptor,
	services *[]*protomodel.ServiceDescriptor,
) {
	*messages = append(*messages, file.AllMessages...)
	*enums = append(*enums, file.AllEnums...)
	*services = append(*services, file.Services...)

	for _, m := range file.AllMessages {
		g.includeUnsituatedDependencies(messages, enums, m, file.Matter.Mode == protomodel.ModePackage)
	}
}

func (g *htmlGenerator) generatePerFileOutput(filesToGen map[*protomodel.FileDescriptor]bool, pkg *protomodel.PackageDescriptor,
	response *plugin.CodeGeneratorResponse,
) {
	// We need to produce a file for each non-hidden file in this package.

	// Decide which types need to be included in the generated file.
	// This will be all the types in the fileToGen input files, along with any
	// dependent types which are located in files that don't have
	// a known location on the web.

	for _, file := range pkg.Files {
		if _, ok := filesToGen[file]; ok {
			g.currentFrontMatterProvider = file
			messages := []*protomodel.MessageDescriptor{}
			enums := []*protomodel.EnumDescriptor{}
			services := []*protomodel.ServiceDescriptor{}

			g.getFileContents(file, &messages, &enums, &services)

			rf := g.generateFile(file, messages, enums, services)
			rf.Name = getPerFileName(file)
			response.File = append(response.File, &rf)
		}
	}
}

func (g *htmlGenerator) generatePerPackageOutput(filesToGen map[*protomodel.FileDescriptor]bool, pkg *protomodel.PackageDescriptor,
	response *plugin.CodeGeneratorResponse,
) {
	// We need to produce a file for this package.

	// Decide which types need to be included in the generated file.
	// This will be all the types in the fileToGen input files, along with any
	// dependent types which are located in packages that don't have
	// a known location on the web.
	messages := []*protomodel.MessageDescriptor{}
	enums := []*protomodel.EnumDescriptor{}
	services := []*protomodel.ServiceDescriptor{}

	for _, file := range pkg.Files {
		if _, ok := filesToGen[file]; ok {
			g.getFileContents(file, &messages, &enums, &services)
		}
	}

	rf := g.generateFile(pkg.FileDesc(), messages, enums, services)
	rf.Name = getPerPackageName(pkg.Name, pkg.FileDesc())
	response.File = append(response.File, &rf)
}

func (g *htmlGenerator) generateOutput(filesToGen map[*protomodel.FileDescriptor]bool) (*plugin.CodeGeneratorResponse, error) {
	// process each package; we produce one output file per package
	supported := uint64(plugin.CodeGeneratorResponse_FEATURE_PROTO3_OPTIONAL)
	response := plugin.CodeGeneratorResponse{
		SupportedFeatures: &supported,
	}

	for _, pkg := range g.model.Packages {
		g.currentPackage = pkg
		g.currentFrontMatterProvider = pkg.FileDesc()

		filteredFiles := map[*protomodel.FileDescriptor]bool{}

		// Set the mode. Supported configurations:
		// * All unset. Defaults to ModeFile
		// * Some set to the same <mode>, others unset. All get configured to <mode>
		// * A mix of one <mode>, ModeNone, and others unset. ModeNone are filtered out, rest are configured to <mode>

		mode := protomodel.ModeUnset
		for _, file := range pkg.Files {
			if mode == protomodel.ModeUnset {
				// No mode set, we assume this file dictates the mode for the rest
				mode = file.Matter.Mode
			} else if mode == protomodel.ModeNone && file.Matter.Mode != protomodel.ModeUnset {
				// Mode was already set to none, but we overrode it. This allows single files opting out
				mode = file.Matter.Mode
			} else if file.Matter.Mode != protomodel.ModeUnset && file.Matter.Mode != mode && file.Matter.Mode != protomodel.ModeNone {
				return nil, fmt.Errorf("all files in a package must have the same mode; have %q got %q (in %v)", mode, file.Matter.Mode, *file.Name)
			}
		}

		for _, file := range pkg.Files {
			fileMode := file.Matter.Mode
			if fileMode == protomodel.ModeUnset {
				fileMode = mode
			}
			if fileMode == protomodel.ModeNone {
				continue
			}
			if _, ok := filesToGen[file]; ok {
				filteredFiles[file] = true
			}
		}

		if len(filteredFiles) > 0 {
			switch mode {
			case protomodel.ModeFile, protomodel.ModeUnset:
				g.generatePerFileOutput(filteredFiles, pkg, &response)
			case protomodel.ModePackage:
				g.generatePerPackageOutput(filteredFiles, pkg, &response)
			case protomodel.ModeNone:
			}
		}
	}

	if g.warningsAsErrors && g.numWarnings > 0 {
		return nil, fmt.Errorf("treating %d warnings as errors", g.numWarnings)
	}

	return &response, nil
}

func (g *htmlGenerator) descLocation(desc protomodel.CoreDesc, isPackage bool) string {
	if !isPackage {
		return desc.FileDesc().Matter.HomeLocation
	}
	if desc.PackageDesc().FileDesc() != nil {
		return desc.PackageDesc().FileDesc().Matter.HomeLocation
	}
	return ""
}

func (g *htmlGenerator) hasName(descs []*protomodel.MessageDescriptor, name string) bool {
	for _, desc := range descs {
		if g.relativeName(desc) == name {
			return true
		}
	}
	return false
}

func (g *htmlGenerator) includeUnsituatedDependencies(messages *[]*protomodel.MessageDescriptor,
	enums *[]*protomodel.EnumDescriptor,
	msg *protomodel.MessageDescriptor,
	isPackage bool,
) {
	for _, field := range msg.Fields {
		switch f := field.FieldType.(type) {
		case *protomodel.MessageDescriptor:
			// A package without a known documentation location is included in the output.
			if g.descLocation(field.FieldType, isPackage) == "" {
				name := g.relativeName(f)
				if !g.hasName(*messages, name) {
					*messages = append(*messages, f)
					g.includeUnsituatedDependencies(messages, enums, msg, isPackage)
				}
			}
		case *protomodel.EnumDescriptor:
			if g.descLocation(field.FieldType, isPackage) == "" {
				*enums = append(*enums, f)
			}
		}
	}
}

func getPerFileName(file *protomodel.FileDescriptor) *string {
	return proto.String(strings.TrimSuffix(file.GetName(), filepath.Ext(file.GetName())) + ".pb.html")
}

func getPerPackageName(name string, file *protomodel.FileDescriptor) *string {
	return proto.String(filepath.Join(filepath.Dir(file.GetName()), name+".pb.html"))
}

// Generate a package documentation file or a collection of cross-linked files.
func (g *htmlGenerator) generateFile(top *protomodel.FileDescriptor, messages []*protomodel.MessageDescriptor,
	enums []*protomodel.EnumDescriptor, services []*protomodel.ServiceDescriptor,
) plugin.CodeGeneratorResponse_File {
	g.buffer.Reset()

	var typeList []string
	var serviceList []string

	messagesMap := map[string]*protomodel.MessageDescriptor{}
	for _, msg := range messages {
		// Don't generate virtual messages for maps.
		if msg.GetOptions().GetMapEntry() {
			continue
		}

		if msg.IsHidden() {
			continue
		}

		absName := g.absoluteName(msg)
		known := wellKnownTypes[absName]
		if known != "" {
			continue
		}

		name := g.relativeName(msg)
		typeList = append(typeList, name)
		messagesMap[name] = msg
	}

	enumMap := map[string]*protomodel.EnumDescriptor{}
	for _, enum := range enums {
		if enum.IsHidden() {
			continue
		}

		absName := g.absoluteName(enum)
		known := wellKnownTypes[absName]
		if known != "" {
			continue
		}

		name := g.relativeName(enum)

		if _, f := enumMap[name]; f {
			continue
		}
		typeList = append(typeList, name)
		enumMap[name] = enum
	}

	// Sort the typeList in dotted name order.
	// For each type, iterate through the rest of the list and add any other
	// types that start with that prefix. Ignore any that have been seen already.
	seen := make(map[string]bool)
	var sortedTypes []string

	// Add a type, and any types that are children of that type
	// (as expressed as MetricsOverrides.TagOverride.Operation)
	var addKey func(string)
	addKey = func(key string) {
		if seen[key] {
			return
		}

		seen[key] = true

		sortedTypes = append(sortedTypes, key)

		// Find any children of this key and add them next
		for _, name := range typeList {
			if strings.HasPrefix(name, key+".") {
				addKey(name)
			}
		}
	}

	// Create sorted version of the typeList
	for _, name := range typeList {
		addKey(name)
	}

	// replace with sorted version
	typeList = sortedTypes

	servicesMap := map[string]*protomodel.ServiceDescriptor{}
	for _, svc := range services {
		if svc.IsHidden() {
			continue
		}

		name := g.relativeName(svc)
		serviceList = append(serviceList, name)
		servicesMap[name] = svc
	}

	numKinds := 0
	if len(typeList) > 0 {
		numKinds++
	}
	if len(serviceList) > 0 {
		numKinds++
	}

	// if there's more than one kind of thing, divide the output in groups
	g.grouping = numKinds > 1

	g.generateFileHeader(top, len(typeList)+len(serviceList))

	if len(serviceList) > 0 {
		if g.grouping {
			g.emit("<h2 id=\"Services\">Services</h2>")
		}

		for _, name := range serviceList {
			service := servicesMap[name]
			g.generateService(service)
		}
	}

	if len(typeList) > 0 {
		if g.grouping {
			g.emit("<h2 id=\"Types\">Types</h2>")
		}

		for _, name := range typeList {
			if e, ok := enumMap[name]; ok {
				g.generateEnum(e)
			} else if m, ok := messagesMap[name]; ok {
				g.generateMessage(m)
			}
		}
	}

	g.generateFileFooter()

	return plugin.CodeGeneratorResponse_File{
		Content: proto.String(g.buffer.String()),
	}
}

func (g *htmlGenerator) generateFileHeader(top *protomodel.FileDescriptor, numEntries int) {
	name := g.currentPackage.Name
	if g.mode == htmlFragmentWithFrontMatter {
		g.emit("---")

		if top != nil && top.Matter.Title != "" {
			g.emit("title: ", top.Matter.Title)
		} else {
			g.emit("title: ", name)
		}

		if top != nil && top.Matter.Overview != "" {
			g.emit("overview: ", top.Matter.Overview)
		}

		if top != nil && top.Matter.Description != "" {
			g.emit("description: ", top.Matter.Description)
		}

		if top != nil && top.Matter.HomeLocation != "" {
			g.emit("location: ", top.Matter.HomeLocation)
		}

		g.emit("layout: protoc-gen-docs")
		g.emit("generator: protoc-gen-docs")

		// emit additional custom front-matter fields
		if g.perFile {
			if top != nil {
				for _, fm := range top.Matter.Extra {
					g.emit(fm)
				}
			}
		} else {
			// Front matter may be in any of the package's files.
			for _, file := range g.currentPackage.Files {
				for _, fm := range file.Matter.Extra {
					g.emit(fm)
				}
			}
		}

		g.emit("number_of_entries: ", strconv.Itoa(numEntries))
		g.emit("---")
	} else if g.mode == htmlPage {
		g.emit("<!DOCTYPE html>")
		g.emit("<html itemscope itemtype=\"https://schema.org/WebPage\">")
		g.emit("<!-- Generated by protoc-gen-docs -->")
		g.emit("<head>")
		g.emit("<meta charset=\"utf-8'>")
		g.emit("<meta name=\"viewport' content=\"width=device-width, initial-scale=1, shrink-to-fit=no\">")

		if top != nil && top.Matter.Title != "" {
			g.emit("<meta name=\"title\" content=\"", top.Matter.Title, "\">")
			g.emit("<meta name=\"og:title\" content=\"", top.Matter.Title, "\">")
			g.emit("<title>", top.Matter.Title, "</title>")
		}

		if top != nil && top.Matter.Overview != "" {
			g.emit("<meta name=\"description\" content=\"", top.Matter.Overview, "\">")
			g.emit("<meta name=\"og:description\" content=\"", top.Matter.Overview, "\">")
		} else if top != nil && top.Matter.Description != "" {
			g.emit("<meta name=\"description\" content=\"", top.Matter.Description, "\">")
			g.emit("<meta name=\"og:description\" content=\"", top.Matter.Description, "\">")
		}

		if g.customStyleSheet != "" {
			g.emit("<link rel=\"stylesheet\" href=\"" + g.customStyleSheet + "\">")
		} else {
			g.emit(htmlStyle)
		}

		g.emit("</head>")
		g.emit("<body>")
		if top != nil && top.Matter.Title != "" {
			g.emit("<h1>", top.Matter.Title, "</h1>")
		}
	} else if g.mode == htmlFragment {
		g.emit("<!-- Generated by protoc-gen-docs -->")
		if top != nil && top.Matter.Title != "" {
			g.emit("<h1>", top.Matter.Title, "</h1>")
		}
	}

	if g.perFile {
		if top != nil {
			g.generateComment(top.Matter.Location, name)
		}
	} else {
		g.generateComment(g.currentPackage.Location(), name)
	}
}

func (g *htmlGenerator) generateFileFooter() {
	if g.mode == htmlPage {
		g.emit("</body>")
		g.emit("</html>")
	}
}

func (g *htmlGenerator) generateSectionHeading(desc protomodel.CoreDesc) {
	class := ""
	if desc.Class() != "" {
		class = desc.Class() + " "
	}

	name := g.relativeName(desc)
	shortName := name

	if idx := strings.LastIndex(name, "."); idx != -1 {
		shortName = name[idx+1:]
	}

	depth := 2
	depth += min(4, strings.Count(name, "."))
	if g.grouping {
		depth++
	}
	heading := fmt.Sprintf("h%d", depth)

	g.emit("<", heading, " id=\"", normalizeID(name), "\">", shortName, "</", heading, ">")

	if class != "" {
		g.emit("<section class=\"", class, "\">")
	} else {
		g.emit("<section>")
	}
}

func (g *htmlGenerator) generateSectionTrailing() {
	g.emit("</section>")
}

func (g *htmlGenerator) generateMessage(message *protomodel.MessageDescriptor) {
	g.generateSectionHeading(message)
	g.generateComment(message.Location(), message.GetName())

	if len(message.Fields) > 0 {
		g.emit("<table class=\"message-fields\">")
		g.emit("<thead>")
		g.emit("<tr>")
		g.emit("<th>Field</th>")
		g.emit("<th>Description</th>")
		g.emit("</tr>")
		g.emit("</thead>")
		g.emit("<tbody>")

		// list the active entries first, then the deprecated ones
		dep := false
		for {
			var oneof int32 = -1
			for _, field := range message.Fields {
				if field.IsHidden() {
					continue
				}

				if (field.Options != nil && field.Options.GetDeprecated() != dep) ||
					(field.Options == nil && dep) {
					continue
				}

				fieldName := *field.Name
				if g.camelCaseFields {
					fieldName = camelCase(*field.Name)
				}

				fieldTypeName := g.fieldTypeName(field)

				class := ""
				if field.Options != nil && field.Options.GetDeprecated() {
					class = deprecated
				}

				if field.Class() != "" {
					class = class + field.Class() + " "
				}

				if field.OneofIndex != nil {
					if *field.OneofIndex != oneof {
						class += "oneof oneof-start"
						oneof = *field.OneofIndex
					} else {
						class += "oneof"
					}
				}

				required := false
				if field.Options != nil {
					fb := getFieldBehavior(field.Options)
					for _, e := range fb {
						if e == annotations.FieldBehavior_REQUIRED {
							required = true
						}
					}
				}

				id := normalizeID(g.relativeName(field))
				if class != "" {
					g.emit(`<tr id="`, id, `" class="`, class, `">`)
				} else {
					g.emit(`<tr id="`, id, `">`)
				}
				fieldLink := `<a href="#` + id + "\">" + fieldName + "</a>"

				// field
				g.emit("<td><div class=\"field\"><div class=\"name\"><code>", fieldLink, "</code></div>")
				// type
				g.emit("<div class=\"type\">", g.linkify(field.FieldType, fieldTypeName, true), "</div>")
				// required
				if required {
					g.emit("<div class=\"required\">Required</div>")
				}
				g.emit("</div></td>")
				g.emit("<td>")

				g.generateComment(field.Location(), field.GetName())

				g.emit("</td>")
				g.emit("</tr>")
			}

			if dep {
				break
			}
			dep = true
		}
		g.emit("</tbody>")
		g.emit("</table>")
	}

	g.generateSectionTrailing()
}

func (g *htmlGenerator) generateEnum(enum *protomodel.EnumDescriptor) {
	g.generateSectionHeading(enum)
	g.generateComment(enum.Location(), enum.GetName())

	if len(enum.Values) > 0 {
		g.emit("<table class=\"enum-values\">")
		g.emit("<thead>")
		g.emit("<tr>")
		g.emit("<th>Name</th>")
		g.emit("<th>Description</th>")
		g.emit("</tr>")
		g.emit("</thead>")
		g.emit("<tbody>")

		// list the active entries first, then the deprecated ones
		dep := false
		for {
			for _, v := range enum.Values {
				if v.IsHidden() {
					continue
				}

				if (v.Options != nil && v.Options.GetDeprecated() != dep) ||
					(v.Options == nil && dep) {
					continue
				}

				name := *v.Name

				class := ""
				if v.Options != nil && v.Options.GetDeprecated() {
					class = deprecated
				}

				if v.Class() != "" {
					class = class + v.Class() + " "
				}

				id := normalizeID(g.relativeName(v))
				if class != "" {
					g.emit(`<tr id="`, id, `" class="`, class, `">`)
				} else {
					g.emit(`<tr id="`, id, `">`)
				}
				fieldLink := `<a href="#` + id + "\">" + name + "</a>"
				g.emit("<td><code>", fieldLink, "</code></td>")
				g.emit("<td>")

				g.generateComment(v.Location(), name)

				g.emit("</td>")
				g.emit("</tr>")
			}

			if dep {
				break
			}
			dep = true
		}
		g.emit("</tbody>")
		g.emit("</table>")
	}

	g.generateSectionTrailing()
}

func (g *htmlGenerator) generateService(service *protomodel.ServiceDescriptor) {
	g.generateSectionHeading(service)
	g.generateComment(service.Location(), service.GetName())

	// list the active entries first, then the deprecated ones
	dep := false
	for {
		for _, method := range service.Methods {
			if method.IsHidden() {
				continue
			}

			if (method.Options != nil && method.Options.GetDeprecated() != dep) ||
				(method.Options == nil && dep) {
				continue
			}

			class := ""
			if method.Options != nil && method.Options.GetDeprecated() {
				class = deprecated
			}

			if method.Class() != "" {
				class = class + method.Class() + " "
			}

			if class != "" {
				g.emit("<pre id=\"", normalizeID(g.relativeName(method)), "\" class=\"", class, "\"><code class=\"language-proto\">rpc ",
					method.GetName(), "(", g.relativeName(method.Input), ") returns (", g.relativeName(method.Output), ")")
			} else {
				g.emit("<pre id=\"", normalizeID(g.relativeName(method)), "\"><code class=\"language-proto\">rpc ",
					method.GetName(), "(", g.relativeName(method.Input), ") returns (", g.relativeName(method.Output), ")")
			}
			g.emit("</code></pre>")

			g.generateComment(method.Location(), method.GetName())
		}

		if dep {
			break
		}
		dep = true
	}

	g.generateSectionTrailing()
}

// emit prints the arguments to the generated output.
func (g *htmlGenerator) emit(str ...string) {
	for _, s := range str {
		g.buffer.WriteString(s)
	}
	g.buffer.WriteByte('\n')
}

var typeLinkPattern = regexp.MustCompile(`\[[^]]*]\[[^]]*]`)

func (g *htmlGenerator) generateComment(loc protomodel.LocationDescriptor, name string) {
	com := loc.GetLeadingComments()
	if com == "" {
		com = loc.GetTrailingComments()
		if com == "" {
			g.warn(loc, 0, "no comment found for %s", name)
			return
		}
	}

	text := strings.TrimSuffix(com, "\n")
	lines := strings.Split(text, "\n")
	if len(lines) > 0 {
		// Based on the amount of spacing at the start of the first line,
		// remove that many characters at the beginning of every line in the comment.
		// This is so we don't inject extra spaces in any preformatted blocks included
		// in the comments
		pad := 0
		for i, ch := range lines[0] {
			if !unicode.IsSpace(ch) {
				pad = i
				break
			}
		}

		for i := 0; i < len(lines); i++ {
			l := lines[i]
			if len(l) > pad {
				skip := 0
				var ch rune
				for skip, ch = range l {
					if !unicode.IsSpace(ch) {
						break
					}

					if skip == pad {
						break
					}
				}
				lines[i] = l[skip:]
			}
		}

		// now, adjust any headers included in the comment to correspond to the right
		// level, based on the heading level of the surrounding content
		for i := 0; i < len(lines); i++ {
			l := lines[i]
			if strings.HasPrefix(l, "#") {
				if g.grouping {
					lines[i] = "###" + l
				} else {
					lines[i] = "##" + l
				}
			}
		}

		// elide HTML comment blocks
		for i := 0; i < len(lines); i++ {
			commentStart := strings.Index(lines[i], "<!--")
			if commentStart < 0 {
				continue
			}

			commentEnd := strings.Index(lines[i][commentStart+3:], "-->")
			if commentEnd >= 0 {
				// strip out the commented portion
				lines[i] = lines[i][:commentStart] + lines[i][commentEnd+3:]
				i-- // run the line through the check again
				continue
			}

			lines[i] = lines[i][:commentStart]

			// find end
			for i++; i < len(lines); i++ {
				commentEnd = strings.Index(lines[i], "-->")
				if commentEnd >= 0 {
					// strip out the commented portion
					lines[i] = lines[i][commentEnd+3:]
					i-- // run the line through the check again
					break
				}
				lines[i] = ""
			}
		}

		// find any type links of the form [name][type] and turn
		// them into normal HTML links
		for i := 0; i < len(lines); i++ {
			lines[i] = typeLinkPattern.ReplaceAllStringFunc(lines[i], func(match string) string {
				end := 0
				for match[end] != ']' {
					end++
				}

				linkName := match[1:end]
				typeName := match[end+2 : len(match)-1]

				if o, ok := g.model.AllDescByName["."+typeName]; ok {
					return g.linkify(o, linkName, false)
				}

				if l, ok := wellKnownTypes[typeName]; ok {
					return "<a href=\"" + l + "\">" + linkName + "</a>"
				}

				g.warn(loc, -(len(lines) - i), "unresolved type link [%s][%s]", linkName, typeName)

				return "*" + linkName + "*"
			})
		}
	}

	// remove "Required. " and "Optional. "
	for i := 0; i < len(lines); i++ {
		lines[i] = regexp.MustCompile(`^Required. `).ReplaceAllString(lines[i], "")
		lines[i] = regexp.MustCompile(`^Optional. `).ReplaceAllString(lines[i], "")
	}

	lines = FilterInPlace(lines, skipLine)
	text = strings.Join(lines, "\n")

	if g.speller != nil {
		preBlock := false
		for linenum, line := range lines {
			trimmed := strings.Trim(line, " ")
			if strings.HasPrefix(trimmed, "```") {
				preBlock = !preBlock
				continue
			}

			if preBlock {
				continue
			}

			line := sanitize(line)

			words := g.speller.Split(line)
			for _, word := range words {
				if !g.speller.Spell(word) {
					g.warn(loc, -(len(lines) - linenum), "%s is misspelled", word)
				}
			}
		}
	}

	// turn the comment from markdown into HTML
	result := markdown.Run([]byte(text))

	g.buffer.Write(result)
	g.buffer.WriteByte('\n')
}

func skipLine(line string) bool {
	// Lots of things use +xyz comments to customize types, strip from docs.
	return !strings.HasPrefix(line, "+")
}

var (
	stripCodeBlocks   = regexp.MustCompile("(`.*`)")
	stripMarkdownURLs = regexp.MustCompile(`\[.*\]\((.*)\)`)
	stripHTMLURLs     = regexp.MustCompile(`(<a href=".*">)`)
)

func sanitize(line string) string {
	// strip out any embedded code blocks and URLs
	line = stripMarkdownURLs.ReplaceAllString(line, "")
	line = stripHTMLURLs.ReplaceAllString(line, "")
	line = stripCodeBlocks.ReplaceAllString(line, "")
	return line
}

// well-known types whose documentation we can refer to
var wellKnownTypes = map[string]string{
	"google.protobuf.Duration":    "https://developers.google.com/protocol-buffers/docs/reference/google.protobuf#duration",
	"google.protobuf.Timestamp":   "https://developers.google.com/protocol-buffers/docs/reference/google.protobuf#timestamp",
	"google.protobuf.Any":         "https://developers.google.com/protocol-buffers/docs/reference/google.protobuf#any",
	"google.protobuf.BytesValue":  "https://developers.google.com/protocol-buffers/docs/reference/google.protobuf#bytesvalue",
	"google.protobuf.StringValue": "https://developers.google.com/protocol-buffers/docs/reference/google.protobuf#stringvalue",
	"google.protobuf.BoolValue":   "https://developers.google.com/protocol-buffers/docs/reference/google.protobuf#boolvalue",
	"google.protobuf.Int32Value":  "https://developers.google.com/protocol-buffers/docs/reference/google.protobuf#int32value",
	"google.protobuf.Int64Value":  "https://developers.google.com/protocol-buffers/docs/reference/google.protobuf#int64value",
	"google.protobuf.Uint32Value": "https://developers.google.com/protocol-buffers/docs/reference/google.protobuf#uint32value",
	"google.protobuf.Uint64Value": "https://developers.google.com/protocol-buffers/docs/reference/google.protobuf#uint64value",
	"google.protobuf.FloatValue":  "https://developers.google.com/protocol-buffers/docs/reference/google.protobuf#floatvalue",
	"google.protobuf.DoubleValue": "https://developers.google.com/protocol-buffers/docs/reference/google.protobuf#doublevalue",
	"google.protobuf.Empty":       "https://developers.google.com/protocol-buffers/docs/reference/google.protobuf#empty",
	"google.protobuf.EnumValue":   "https://developers.google.com/protocol-buffers/docs/reference/google.protobuf#enumvalue",
	"google.protobuf.ListValue":   "https://developers.google.com/protocol-buffers/docs/reference/google.protobuf#listvalue",
	"google.protobuf.NullValue":   "https://developers.google.com/protocol-buffers/docs/reference/google.protobuf#nullvalue",
	"google.protobuf.Struct":      "https://developers.google.com/protocol-buffers/docs/reference/google.protobuf#struct",
}

func (g *htmlGenerator) linkify(o protomodel.CoreDesc, name string, onlyLastComponent bool) string {
	if o == nil {
		return name
	}

	if msg, ok := o.(*protomodel.MessageDescriptor); ok && msg.GetOptions().GetMapEntry() {
		return name
	}

	displayName := name
	if onlyLastComponent {
		index := strings.LastIndex(name, ".")
		if index > 0 && index < len(name)-1 {
			displayName = name[index+1:]
		}
	}

	known := wellKnownTypes[g.absoluteName(o)]
	if known != "" {
		return "<a href=\"" + known + "\">" + displayName + "</a>"
	}

	if !o.IsHidden() {
		// is there a file-specific home location?
		loc := o.FileDesc().Matter.HomeLocation

		// if there isn't a file-specific home location, see if there's a package-wide location
		if loc == "" && o.PackageDesc().FileDesc() != nil {
			loc = o.PackageDesc().FileDesc().Matter.HomeLocation
		}

		if loc != "" && (g.currentFrontMatterProvider == nil || loc != g.currentFrontMatterProvider.Matter.HomeLocation) {
			return "<a href=\"" + loc + "#" + normalizeID(protomodel.DottedName(o)) + "\">" + displayName + "</a>"
		}
	}

	return "<a href=\"#" + normalizeID(g.relativeName(o)) + "\">" + displayName + "</a>"
}

func (g *htmlGenerator) warn(loc protomodel.LocationDescriptor, lineOffset int, format string, args ...interface{}) {
	if g.genWarnings {
		place := ""
		if loc.SourceCodeInfo_Location != nil && len(loc.Span) >= 2 {
			if lineOffset < 0 {
				place = fmt.Sprintf("%s:%d: ", loc.File.GetName(), loc.Span[0]+int32(lineOffset)+1)
			} else {
				place = fmt.Sprintf("%s:%d:%d: ", loc.File.GetName(), loc.Span[0]+1, loc.Span[1]+1)
			}
		}

		_, _ = fmt.Fprintf(os.Stderr, place+format+"\n", args...)
		g.numWarnings++
	}
}

func (g *htmlGenerator) relativeName(desc protomodel.CoreDesc) string {
	typeName := protomodel.DottedName(desc)
	if desc.PackageDesc() == g.currentPackage {
		return typeName
	}

	return desc.PackageDesc().Name + "." + typeName
}

func (g *htmlGenerator) absoluteName(desc protomodel.CoreDesc) string {
	typeName := protomodel.DottedName(desc)
	return desc.PackageDesc().Name + "." + typeName
}

func (g *htmlGenerator) fieldTypeName(field *protomodel.FieldDescriptor) string {
	name := "n/a"
	switch *field.Type {
	case descriptor.FieldDescriptorProto_TYPE_DOUBLE:
		name = "double"

	case descriptor.FieldDescriptorProto_TYPE_FLOAT:
		name = "float"

	case descriptor.FieldDescriptorProto_TYPE_INT32, descriptor.FieldDescriptorProto_TYPE_SINT32, descriptor.FieldDescriptorProto_TYPE_SFIXED32:
		name = "int32"

	case descriptor.FieldDescriptorProto_TYPE_INT64, descriptor.FieldDescriptorProto_TYPE_SINT64, descriptor.FieldDescriptorProto_TYPE_SFIXED64:
		name = "int64"

	case descriptor.FieldDescriptorProto_TYPE_UINT64, descriptor.FieldDescriptorProto_TYPE_FIXED64:
		name = "uint64"

	case descriptor.FieldDescriptorProto_TYPE_UINT32, descriptor.FieldDescriptorProto_TYPE_FIXED32:
		name = "uint32"

	case descriptor.FieldDescriptorProto_TYPE_BOOL:
		name = "bool"

	case descriptor.FieldDescriptorProto_TYPE_STRING:
		name = "string"

	case descriptor.FieldDescriptorProto_TYPE_MESSAGE:
		msg := field.FieldType.(*protomodel.MessageDescriptor)
		if msg.GetOptions().GetMapEntry() {
			keyType := g.fieldTypeName(msg.Fields[0])
			valType := g.linkify(msg.Fields[1].FieldType, g.fieldTypeName(msg.Fields[1]), true)
			return "map&lt;" + keyType + ",&nbsp;" + valType + "&gt;"
		}
		name = g.relativeName(field.FieldType)

	case descriptor.FieldDescriptorProto_TYPE_BYTES:
		name = "bytes"

	case descriptor.FieldDescriptorProto_TYPE_ENUM:
		name = g.relativeName(field.FieldType)
	}

	if field.IsRepeated() {
		name += "[]"
	}

	if field.OneofIndex != nil {
		name += " (oneof)"
	}

	return name
}

/* TODO
func (g *htmlGenerator) fieldYAMLTypeName(field *FieldDescriptor) string {
	name := "n/a"
	switch *field.Type {
	case descriptor.FieldDescriptorProto_TYPE_DOUBLE:
		name = "double"

	case descriptor.FieldDescriptorProto_TYPE_FLOAT:
		name = "float"

	case descriptor.FieldDescriptorProto_TYPE_INT32, descriptor.FieldDescriptorProto_TYPE_SINT32, descriptor.FieldDescriptorProto_TYPE_SFIXED32:
		name = "int32"

	case descriptor.FieldDescriptorProto_TYPE_INT64, descriptor.FieldDescriptorProto_TYPE_SINT64, descriptor.FieldDescriptorProto_TYPE_SFIXED64:
		name = "int64"

	case descriptor.FieldDescriptorProto_TYPE_UINT64, descriptor.FieldDescriptorProto_TYPE_FIXED64:
		name = "uint64"

	case descriptor.FieldDescriptorProto_TYPE_UINT32, descriptor.FieldDescriptorProto_TYPE_FIXED32:
		name = "uint32"

	case descriptor.FieldDescriptorProto_TYPE_BOOL:
		name = "bool"

	case descriptor.FieldDescriptorProto_TYPE_STRING:
		name = "string"

	case descriptor.FieldDescriptorProto_TYPE_MESSAGE:
		msg := field.typ.(*MessageDescriptor)
		if msg.GetOptions().GetMapEntry() {
			keyType := g.fieldTypeName(msg.fields[0])
			valType := g.linkify(msg.fields[1].typ, g.fieldTypeName(msg.fields[1]))
			return "map&lt;" + keyType + ",&nbsp;" + valType + "&gt;"
		}
		name = g.relativeName(field.typ)

	case descriptor.FieldDescriptorProto_TYPE_BYTES:
		name = "bytes"

	case descriptor.FieldDescriptorProto_TYPE_ENUM:
		name = "enum(" + g.relativeName(field.typ) + ")"
	}

	return name
}
*/

// camelCase returns the camelCased name.
func camelCase(s string) string {
	b := bytes.Buffer{}
	nextUpper := false
	for _, ch := range s {
		if ch == '_' {
			nextUpper = true
		} else {
			if nextUpper {
				nextUpper = false
				ch = unicode.ToUpper(ch)
			}
			b.WriteRune(ch)
		}
	}

	return b.String()
}

func normalizeID(id string) string {
	id = strings.Replace(id, " ", "-", -1)
	return strings.Replace(id, ".", "-", -1)
}

// nolint: interfacer
func getFieldBehavior(options *descriptor.FieldOptions) []annotations.FieldBehavior {
	b, err := proto.Marshal(options)
	if err != nil {
		return nil
	}
	o := &descriptor.FieldOptions{}
	if err = proto.Unmarshal(b, o); err != nil {
		return nil
	}
	e := proto.GetExtension(o, annotations.E_FieldBehavior)
	s, ok := e.([]annotations.FieldBehavior)
	if !ok {
		return nil
	}
	return s
}

var htmlStyle = `
<style>
    html {
        overflow-y: scroll;
        position: relative;
        min-height: 100%
    }

    body {
        font-family: "Roboto", "Helvetica Neue", Helvetica, Arial, sans-serif;
        color: #535f61
    }

    a {
        color: #466BB0;
        text-decoration: none;
        font-weight: 500
    }

    a:hover, a:focus {
        color: #8ba3d1;
        text-decoration: none;
        font-weight: 500
    }

    a.disabled {
        color: #ccc;
        text-decoration: none;
        font-weight: 500
    }

    table, th, td {
        border: 1px solid #849396;
        padding: .3em
    }

	tr.oneof>td {
		border-bottom: 1px dashed #849396;
		border-top: 1px dashed #849396;
	}

    table {
        border-collapse: collapse
    }

    th {
        color: #fff;
        background-color: #286AC7;
        font-weight: normal
    }

    p {
        font-size: 1rem;
        line-height: 1.5;
        margin: .25em 0
    }

	table p:first-of-type {
		margin-top: 0
	}

	table p:last-of-type {
		margin-bottom: 0
	}

    @media (min-width: 768px) {
        p {
            margin: 1.5em 0
        }
    }

    li, dt, dd {
        font-size: 1rem;
        line-height: 1.5;
        margin: .25em
    }

    ol, ul, dl {
        list-style: initial;
        font-size: 1rem;
        margin: 0 1.5em;
        padding: 0
    }

    li p, dt p, dd p {
        margin: .4em 0
    }

    ol {
        list-style: decimal
    }

    h1, h2, h3, h4, h5, h6 {
        border: 0;
        font-weight: normal
    }

    h1 {
        font-size: 2.5rem;
        color: #286AC7;
        margin: 30px 0
    }

    h2 {
        font-size: 2rem;
        color: #2E2E2E;
        margin-bottom: 20px;
        margin-top: 30px;
        padding-bottom: 10px;
        border-bottom: 1px;
        border-color: #737373;
        border-style: solid
    }

    h3 {
        font-size: 1.85rem;
        font-weight: 500;
        color: #404040;
        letter-spacing: 1px;
        margin-bottom: 20px;
        margin-top: 30px
    }

    h4 {
        font-size: 1.85rem;
        font-weight: 500;
        margin: 30px 0 20px;
        color: #404040
    }

    em {
        font-style: italic
    }

    strong {
        font-weight: bold
    }

    blockquote {
        display: block;
        margin: 1em 3em;
        background-color: #f8f8f8
    }

	section {
		padding-left: 2em;
	}

	code {
		color: red;
	}

	.deprecated {
		background: silver;
	}

	.experimental {
		background: yellow;
	}
</style>
`

func FilterInPlace[E any](s []E, f func(E) bool) []E {
	n := 0
	for _, val := range s {
		if f(val) {
			s[n] = val
			n++
		}
	}
	s = s[:n]
	return s
}
