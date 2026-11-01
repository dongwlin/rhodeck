// Package switchtheme provides the strict switch_theme@1 adapter for the
// SwitchTheme Core task. New returns a control.TaskAdapter for explicit registration
// in control.Config.Adapters or control.PreparePlan. A task selects the adapter
// through Task.Adapter; the Core type name alone never selects it.
//
// Preparation validates and compiles local input without filesystem, native or
// device effects. Result interpretation records only associated Core facts;
// acceptance and task-chain completion do not establish a business objective.
package switchtheme
