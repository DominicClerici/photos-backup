/**
 * The ways into the archive that are not "everything, by date".
 *
 * The three sections and their menus are shared with the vault: a bucket's
 * front page is this screen over what is inside it, drawn by these same
 * components with a `bucket` given — see `src/vault/BucketView`. Which is why
 * the grids, the rows and their sheets are exported and not only the screen.
 */
export { AlbumGrid, AlbumMenu } from './AlbumGrid';
export { CategoryList, categoryLabel } from './CategoryList';
export { Collections } from './Collections';
export { CollectionView } from './CollectionView';
export { Cover } from './Cover';
export { CreateAlbumSheet, type CreateAlbumRequest } from './CreateAlbumSheet';
export { PeopleRow, PersonMenu } from './PeopleRow';
