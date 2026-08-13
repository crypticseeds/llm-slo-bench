package report

import _ "embed"

// uPlot is vendored so generated reports remain self-contained and work from file://.
//
//go:embed assets/uPlot.iife.min.js
var uPlotJS string

//go:embed assets/uPlot.min.css
var uPlotCSS string

//go:embed assets/uPlot.LICENSE
var uPlotLicense string
