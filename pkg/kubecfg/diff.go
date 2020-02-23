// Copyright 2017 The kubecfg authors
//
//
//    Licensed under the Apache License, Version 2.0 (the "License");
//    you may not use this file except in compliance with the License.
//    You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
//    Unless required by applicable law or agreed to in writing, software
//    distributed under the License is distributed on an "AS IS" BASIS,
//    WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
//    See the License for the specific language governing permissions and
//    limitations under the License.

package kubecfg

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/kubernetes/pkg/kubectl/cmd/util/openapi"
	"os"
	"regexp"
	"sort"

	isatty "github.com/mattn/go-isatty"
	"github.com/sergi/go-diff/diffmatchpatch"
	log "github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"

	"github.com/bitnami/kubecfg/utils"
)

var ErrDiffFound = fmt.Errorf("Differences found.")

// Matches all the line starts on a diff text, which is where we put diff markers and indent
var DiffLineStart = regexp.MustCompile("(^|\n)(.)")

var DiffKeyValue = regexp.MustCompile(`"([-._a-zA-Z0-9]+)":\s"([[:alnum:]=+]+)",?`)

// DiffCmd represents the diff subcommand
type DiffCmd struct {
	Client           dynamic.Interface
	Mapper           meta.RESTMapper
	Discovery        discovery.DiscoveryInterface
	DefaultNamespace string
	OmitSecrets      bool

	DiffStrategy string
}

func (c DiffCmd) Run(apiObjects []*unstructured.Unstructured, out io.Writer) error {
	sort.Sort(utils.AlphabeticalOrder(apiObjects))

	dmp := diffmatchpatch.New()
	diffFound := false
	schemaDoc, err := c.Discovery.OpenAPISchema()
	if err != nil {
		return err
	}
	schemaResources, err := openapi.NewOpenAPIData(schemaDoc)
	if err != nil {
		return err
	}
	for _, obj := range apiObjects {
		desc := fmt.Sprintf("%s %s", utils.ResourceNameFor(c.Mapper, obj), utils.FqName(obj))
		log.Debug("Fetching ", desc)

		client, err := utils.ClientForResource(c.Client, c.Mapper, obj, c.DefaultNamespace)
		if err != nil {
			return err
		}

		if obj.GetName() == "" {
			return fmt.Errorf("Error fetching one of the %s: it does not have a name set", utils.ResourceNameFor(c.Mapper, obj))
		}

		liveObj, err := client.Get(obj.GetName(), metav1.GetOptions{})
		if err != nil && errors.IsNotFound(err) {
			log.Debugf("%s doesn't exist on the server", desc)
			liveObj = nil
		} else if err != nil {
			return fmt.Errorf("Error fetching %s: %v", desc, err)
		}

		fmt.Fprintln(out, "---")
		fmt.Fprintf(out, "- live %s\n+ config %s\n", desc, desc)
		if liveObj == nil {
			fmt.Fprintf(out, "%s doesn't exist on server\n", desc)
			diffFound = true
			continue
		}

		if c.DiffStrategy == "update" {
			schema := schemaResources.LookupResource(obj.GroupVersionKind())
			if !isValidKindSchema(schema) {
				// Invalid schema (eg: custom resource without
				// schema returns trivial type:object with k8s >=1.15)
				log.Debugf("Ignoring invalid schema for %s", obj.GroupVersionKind())
				schema = nil
			}
			mergedObj, err := patch(liveObj, obj, schema)
			if err != nil {
				return err
			}
			if ts := mergedObj.GetCreationTimestamp(); ts.IsZero() {
				liveObj.SetCreationTimestamp(metav1.Time{})
			}
			if apiequality.Semantic.DeepEqual(liveObj, mergedObj) {
				log.Debugf("Not updating %s - unchanged", desc)
				return nil
			}
			liveObjObject := liveObj.Object
			utils.DeleteMetaDataAnnotation(liveObj, AnnotationOrigObject)
			utils.DeleteMetaDataAnnotation(mergedObj, AnnotationOrigObject)
			liveObjText, _ := json.MarshalIndent(liveObjObject, "", "  ")
			mergedObjText, _ := json.MarshalIndent(mergedObj.Object, "", "  ")

			liveObjTextLines, mergedObjTextLines, lines := dmp.DiffLinesToChars(string(liveObjText), string(mergedObjText))

			diff := dmp.DiffMain(
				string(liveObjTextLines),
				string(mergedObjTextLines),
				false)

			diff = dmp.DiffCharsToLines(diff, lines)
			if (len(diff) == 1) && (diff[0].Type == diffmatchpatch.DiffEqual) {
				fmt.Fprintf(out, "%s unchanged\n", desc)
			} else {
				diffFound = true
				text := c.formatDiff(diff, isatty.IsTerminal(os.Stdout.Fd()), c.OmitSecrets && obj.GetKind() == "Secret")
				fmt.Fprintf(out, "%s\n", text)
			}
			return nil
		}

		liveObjObject := liveObj.Object
		if c.DiffStrategy == "subset" {
			liveObjObject = removeMapFields(obj.Object, liveObjObject)
		}

		liveObjText, _ := json.MarshalIndent(liveObjObject, "", "  ")
		objText, _ := json.MarshalIndent(obj.Object, "", "  ")

		liveObjTextLines, objTextLines, lines := dmp.DiffLinesToChars(string(liveObjText), string(objText))

		diff := dmp.DiffMain(
			string(liveObjTextLines),
			string(objTextLines),
			false)

		diff = dmp.DiffCharsToLines(diff, lines)
		if (len(diff) == 1) && (diff[0].Type == diffmatchpatch.DiffEqual) {
			fmt.Fprintf(out, "%s unchanged\n", desc)
		} else {
			diffFound = true
			text := c.formatDiff(diff, isatty.IsTerminal(os.Stdout.Fd()), c.OmitSecrets && obj.GetKind() == "Secret")
			fmt.Fprintf(out, "%s\n", text)
		}
	}

	if diffFound {
		return ErrDiffFound
	}
	return nil
}

// Formats the supplied Diff as a unified-diff-like text with infinite context and optionally colorizes it.
func (c DiffCmd) formatDiff(diffs []diffmatchpatch.Diff, color bool, omitchanges bool) string {
	var buff bytes.Buffer

	for _, diff := range diffs {
		text := diff.Text

		if omitchanges {
			text = DiffKeyValue.ReplaceAllString(text, "$1: <omitted>")
		}
		switch diff.Type {
		case diffmatchpatch.DiffInsert:
			if color {
				_, _ = buff.WriteString("\x1b[32m")
			}
			_, _ = buff.WriteString(DiffLineStart.ReplaceAllString(text, "$1+ $2"))
			if color {
				_, _ = buff.WriteString("\x1b[0m")
			}
		case diffmatchpatch.DiffDelete:
			if color {
				_, _ = buff.WriteString("\x1b[31m")
			}
			_, _ = buff.WriteString(DiffLineStart.ReplaceAllString(text, "$1- $2"))
			if color {
				_, _ = buff.WriteString("\x1b[0m")
			}
		case diffmatchpatch.DiffEqual:
			if !omitchanges {
				_, _ = buff.WriteString(DiffLineStart.ReplaceAllString(text, "$1  $2"))
			}
		}
	}

	return buff.String()
}

// See also feature request for golang reflect pkg at
func isEmptyValue(i interface{}) bool {
	switch v := i.(type) {
	case []interface{}:
		return len(v) == 0
	case []string:
		return len(v) == 0
	case map[string]interface{}:
		return len(v) == 0
	case bool:
		return !v
	case float64:
		return v == 0
	case int64:
		return v == 0
	case string:
		return v == ""
	case nil:
		return true
	default:
		panic(fmt.Sprintf("Found unexpected type %T in json unmarshal (value=%v)", i, i))
	}
}

func removeFields(config, live interface{}) interface{} {
	switch c := config.(type) {
	case map[string]interface{}:
		if live, ok := live.(map[string]interface{}); ok {
			return removeMapFields(c, live)
		}
	case []interface{}:
		if live, ok := live.([]interface{}); ok {
			return removeListFields(c, live)
		}
	}
	return live
}

func removeMapFields(config, live map[string]interface{}) map[string]interface{} {
	result := map[string]interface{}{}
	for k, v1 := range config {
		v2, ok := live[k]
		if !ok {
			// Copy empty value from config, as API won't return them,
			// see https://github.com/bitnami/kubecfg/issues/179
			if isEmptyValue(v1) {
				result[k] = v1
			}
			continue
		}
		result[k] = removeFields(v1, v2)
	}
	return result
}

func removeListFields(config, live []interface{}) []interface{} {
	// If live is longer than config, then the extra elements at the end of the
	// list will be returned as is so they appear in the diff.
	result := make([]interface{}, 0, len(live))
	for i, v2 := range live {
		if len(config) > i {
			result = append(result, removeFields(config[i], v2))
		} else {
			result = append(result, v2)
		}
	}
	return result
}

func istty(w io.Writer) bool {
	if f, ok := w.(*os.File); ok {
		return isatty.IsTerminal(f.Fd())
	}
	return false
}
