/**
 * What is done to a grid rather than drawn by one: the floating sort-and-filter
 * and selection controls, the sheets they open, and the little store that lets
 * a peek inside a `Modal` open a sheet that outlives it.
 *
 * All of it is mounted once, by the root layout — see `Controls`.
 */
export { Controls } from './Controls';
export { askToFile, stopFiling, useFiling, type FileRequest } from './filing';
