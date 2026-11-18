// Who may read armstrong (the-valley's schema/read.cue): patrick alone. A
// reader reads it at an edge only where the owning instance's identity
// registry grants that reader read of armstrong there too.
package read

project:  "armstrong"
identity: "82a20a95f2546404a194993e3b5bdff4"
readers: patrick: true
