/*
Copyright 2014 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package cmd

import (
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/golang/glog"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/kubernetes/pkg/kubectl"
	"k8s.io/kubernetes/pkg/kubectl/cmd/templates"
	cmdutil "k8s.io/kubernetes/pkg/kubectl/cmd/util"
	"k8s.io/kubernetes/pkg/kubectl/resource"
	"k8s.io/kubernetes/pkg/kubectl/util/i18n"
	"k8s.io/kubernetes/pkg/kubectl/validation"
	"k8s.io/kubernetes/pkg/printers"
)

var (
	replaceLong = templates.LongDesc(i18n.T(`
		Replace a resource by filename or stdin.

		JSON and YAML formats are accepted. If replacing an existing resource, the
		complete resource spec must be provided. This can be obtained by

		    $ kubectl get TYPE NAME -o yaml

		Please refer to the models in https://htmlpreview.github.io/?https://github.com/kubernetes/kubernetes/blob/HEAD/docs/api-reference/v1/definitions.html to find if a field is mutable.`))

	replaceExample = templates.Examples(i18n.T(`
		# Replace a pod using the data in pod.json.
		kubectl replace -f ./pod.json

		# Replace a pod based on the JSON passed into stdin.
		cat pod.json | kubectl replace -f -

		# Update a single-container pod's image version (tag) to v4
		kubectl get pod mypod -o yaml | sed 's/\(image: myimage\):.*$/\1:v4/' | kubectl replace -f -

		# Force replace, delete and then re-create the resource
		kubectl replace --force -f ./pod.json`))
)

type ReplaceOpts struct {
	PrintFlags      *printers.PrintFlags
	FileNameOptions *resource.FilenameOptions
	DeleteOptions   *DeleteOptions

	PrintObj func(obj runtime.Object) error

	createAnnotation bool
	changeCause      string
	validate         bool

	Schema      validation.Schema
	Builder     func() *resource.Builder
	BuilderArgs []string

	ShouldRecord func(info *resource.Info) bool

	Namespace        string
	EnforceNamespace bool

	Out    io.Writer
	ErrOut io.Writer
}

func NewCmdReplace(f cmdutil.Factory, out, errOut io.Writer) *cobra.Command {
	options := &ReplaceOpts{
		PrintFlags: printers.NewPrintFlags("replaced"),

		FileNameOptions: &resource.FilenameOptions{},
		DeleteOptions:   NewDeleteOptions(out, errOut),

		Out:    out,
		ErrOut: errOut,
	}

	cmd := &cobra.Command{
		Use: "replace -f FILENAME",
		DisableFlagsInUseLine: true,
		Short:   i18n.T("Replace a resource by filename or stdin"),
		Long:    replaceLong,
		Example: replaceExample,
		Run: func(cmd *cobra.Command, args []string) {
			cmdutil.CheckErr(cmdutil.ValidateOutputArgs(cmd))
			cmdutil.CheckErr(options.Complete(f, cmd, args))
			cmdutil.CheckErr(options.Validate(cmd))
			cmdutil.CheckErr(options.Run())
		},
	}

	options.PrintFlags.AddFlags(cmd)

	usage := "to use to replace the resource."
	cmdutil.AddFilenameOptionFlags(cmd, options.FileNameOptions, usage)
	cmd.MarkFlagRequired("filename")
	cmd.Flags().BoolVar(&options.DeleteOptions.ForceDeletion, "force", options.DeleteOptions.ForceDeletion, "Delete and re-create the specified resource")
	cmd.Flags().BoolVar(&options.DeleteOptions.Cascade, "cascade", options.DeleteOptions.Cascade, "Only relevant during a force replace. If true, cascade the deletion of the resources managed by this resource (e.g. Pods created by a ReplicationController).")
	cmd.Flags().IntVar(&options.DeleteOptions.GracePeriod, "grace-period", options.DeleteOptions.GracePeriod, "Only relevant during a force replace. Period of time in seconds given to the old resource to terminate gracefully. Ignored if negative.")
	cmd.Flags().DurationVar(&options.DeleteOptions.Timeout, "timeout", options.DeleteOptions.Timeout, "Only relevant during a force replace. The length of time to wait before giving up on a delete of the old resource, zero means determine a timeout from the size of the object. Any other values should contain a corresponding time unit (e.g. 1s, 2m, 3h).")
	cmdutil.AddValidateFlags(cmd)
	cmdutil.AddApplyAnnotationFlags(cmd)
	cmdutil.AddRecordFlag(cmd)
	cmdutil.AddInclude3rdPartyFlags(cmd)

	return cmd
}

func (o *ReplaceOpts) Complete(f cmdutil.Factory, cmd *cobra.Command, args []string) error {
	o.validate = cmdutil.GetFlagBool(cmd, "validate")
	o.changeCause = f.Command(cmd, false)
	o.createAnnotation = cmdutil.GetFlagBool(cmd, cmdutil.ApplyAnnotationsFlag)

	o.ShouldRecord = func(info *resource.Info) bool {
		return cmdutil.ShouldRecord(cmd, info)
	}

	printer, err := o.PrintFlags.ToPrinter()
	if err != nil {
		return err
	}
	o.PrintObj = func(obj runtime.Object) error {
		return printer.PrintObj(obj, o.Out)
	}

	// complete delete options
	// TODO(juanvallejo): Turn these fields in a DeleteFlags struct, similar to PrintFlags
	//Replace will create a resource if it doesn't exist already, so ignore not found error
	o.DeleteOptions.IgnoreNotFound = true
	o.DeleteOptions.Reaper = f.Reaper

	if o.PrintFlags.OutputFormat != nil {
		o.DeleteOptions.Output = *o.PrintFlags.OutputFormat
	}

	if o.DeleteOptions.GracePeriod == 0 {
		// To preserve backwards compatibility, but prevent accidental data loss, we convert --grace-period=0
		// into --grace-period=1 and wait until the object is successfully deleted.
		o.DeleteOptions.GracePeriod = 1
		o.DeleteOptions.WaitForDeletion = true
	}

	schema, err := f.Validator(o.validate)
	if err != nil {
		return err
	}

	o.Schema = schema
	o.Builder = f.NewBuilder
	o.BuilderArgs = args

	o.Namespace, o.EnforceNamespace, err = f.DefaultNamespace()
	if err != nil {
		return err
	}

	return nil
}

func (o *ReplaceOpts) Validate(cmd *cobra.Command) error {
	if o.DeleteOptions.GracePeriod >= 0 && !o.DeleteOptions.ForceDeletion {
		return fmt.Errorf("--grace-period must have --force specified")
	}

	if o.DeleteOptions.Timeout != 0 && !o.DeleteOptions.ForceDeletion {
		return fmt.Errorf("--timeout must have --force specified")
	}

	if cmdutil.IsFilenameSliceEmpty(o.FileNameOptions.Filenames) {
		return cmdutil.UsageErrorf(cmd, "Must specify --filename to replace")
	}

	return nil
}

func (o *ReplaceOpts) Run() error {
	if o.DeleteOptions.ForceDeletion {
		return o.forceReplace()
	}

	r := o.Builder().
		Unstructured().
		Schema(o.Schema).
		ContinueOnError().
		NamespaceParam(o.Namespace).DefaultNamespace().
		FilenameParam(o.EnforceNamespace, o.FileNameOptions).
		Flatten().
		Do()
	if err := r.Err(); err != nil {
		return err
	}

	return o.Result.Visit(func(info *resource.Info, err error) error {
		if err != nil {
			return err
		}

		if err := kubectl.CreateOrUpdateAnnotation(o.createAnnotation, info, cmdutil.InternalVersionJSONEncoder()); err != nil {
			return cmdutil.AddSourceToErr("replacing", info.Source, err)
		}

		if o.ShouldRecord(info) {
			if err := cmdutil.RecordChangeCause(info.Object, o.changeCause); err != nil {
				return cmdutil.AddSourceToErr("replacing", info.Source, err)
			}
		}

		// Serialize the object with the annotation applied.
		obj, err := resource.NewHelper(info.Client, info.Mapping).Replace(info.Namespace, info.Name, true, info.Object)
		if err != nil {
			return cmdutil.AddSourceToErr("replacing", info.Source, err)
		}

		info.Refresh(obj, true)
		return o.PrintObj(info.AsVersioned())
	})
}

func (o *ReplaceOpts) forceReplace() error {
	for i, filename := range o.FileNameOptions.Filenames {
		if filename == "-" {
			tempDir, err := ioutil.TempDir("", "kubectl_replace_")
			if err != nil {
				return err
			}
			defer os.RemoveAll(tempDir)
			tempFilename := filepath.Join(tempDir, "resource.stdin")
			err = cmdutil.DumpReaderToFile(os.Stdin, tempFilename)
			if err != nil {
				return err
			}
			o.FileNameOptions.Filenames[i] = tempFilename
		}
	}

	r := o.Builder().
		Unstructured().
		ContinueOnError().
		NamespaceParam(o.Namespace).DefaultNamespace().
		FilenameParam(o.EnforceNamespace, o.FileNameOptions).
		ResourceTypeOrNameArgs(false, o.BuilderArgs...).RequireObject(false).
		Flatten().
		Do()
	if err := r.Err(); err != nil {
		return err
	}

	var err error

	// By default use a reaper to delete all related resources.
	if o.DeleteOptions.Cascade {
		glog.Warningf("\"cascade\" is set, kubectl will delete and re-create all resources managed by this resource (e.g. Pods created by a ReplicationController). Consider using \"kubectl rolling-update\" if you want to update a ReplicationController together with its Pods.")
		err = o.DeleteOptions.ReapResult(r, o.DeleteOptions.Cascade, false)
	} else {
		err = o.DeleteOptions.DeleteResult(r)
	}

	if timeout == 0 {
		timeout = kubectl.Timeout
	}
	err = r.Visit(func(info *resource.Info, err error) error {
		if err != nil {
			return err
		}

		return wait.PollImmediate(kubectl.Interval, timeout, func() (bool, error) {
			if err := info.Get(); !errors.IsNotFound(err) {
				return false, err
			}
			return true, nil
		})
	})
	if err != nil {
		return err
	}

	r = o.Builder().
		Unstructured().
		Schema(o.Schema).
		ContinueOnError().
		NamespaceParam(o.Namespace).DefaultNamespace().
		FilenameParam(o.EnforceNamespace, o.FileNameOptions).
		Flatten().
		Do()
	err = r.Err()
	if err != nil {
		return err
	}

	count := 0
	err = r.Visit(func(info *resource.Info, err error) error {
		if err != nil {
			return err
		}

		if err := kubectl.CreateOrUpdateAnnotation(o.createAnnotation, info, cmdutil.InternalVersionJSONEncoder()); err != nil {
			return err
		}

		if o.ShouldRecord(info) {
			if err := cmdutil.RecordChangeCause(info.Object, o.changeCause); err != nil {
				return cmdutil.AddSourceToErr("replacing", info.Source, err)
			}
		}

		obj, err := resource.NewHelper(info.Client, info.Mapping).Create(info.Namespace, true, info.Object)
		if err != nil {
			return err
		}

		count++
		info.Refresh(obj, true)
		return o.PrintObj(info.AsVersioned())
	})
	if err != nil {
		return err
	}
	if count == 0 {
		return fmt.Errorf("no objects passed to replace")
	}
	return nil
}
